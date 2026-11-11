// SPDX-License-Identifier: GPL-3.0-only
package k8s

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestListScannableNamespaces(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "prod"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "staging"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "velero"}},
	)

	discovery := NewDiscovery(client, nil)
	ns, err := discovery.ListScannableNamespaces(context.Background(), []string{"kube-system", "velero"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(ns) != 2 || ns[0] != "prod" || ns[1] != "staging" {
		t.Fatalf("expected [prod, staging], got %v", ns)
	}
}

func TestDiscoverReleasesAndUncoveredPVCs(t *testing.T) {
	client := fake.NewSimpleClientset(
		// Deployment for release "app-a" opted-in with preset annotation
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "app-a-web",
				Namespace: "prod",
				Labels: map[string]string{
					HelmManagedByLabel:       "Helm",
					HelmInstanceLabel:        "app-a",
					HelmChartLabel:           "app-a-1.2.3",
					"pvcguardian.io/enabled": "true",
				},
				Annotations: map[string]string{
					"pvcguardian.io/preset": "night",
				},
			},
		},
		// StatefulSet for release "app-b" opted-in
		&appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "app-b-db",
				Namespace: "prod",
				Labels: map[string]string{
					HelmManagedByLabel:       "Helm",
					HelmInstanceLabel:        "app-b",
					HelmChartLabel:           "app-b-0.5.0",
					"pvcguardian.io/enabled": "true",
				},
			},
		},
		// CronJob for release "app-c" NOT opted-in (missing label)
		&batchv1.CronJob{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "app-c-cron",
				Namespace: "prod",
				Labels: map[string]string{
					HelmManagedByLabel: "Helm",
					HelmInstanceLabel:  "app-c",
				},
			},
		},
		// Workload with Helm managed-by but missing instance label
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "orphan-workload",
				Namespace: "prod",
				Labels: map[string]string{
					HelmManagedByLabel: "Helm",
				},
			},
		},
		// PVC for app-a
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "data-app-a",
				Namespace: "prod",
				Labels: map[string]string{
					HelmInstanceLabel: "app-a",
				},
			},
		},
		// Uncovered PVC (manual, no matching release)
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "manual-pvc",
				Namespace: "prod",
			},
		},
	)

	discovery := NewDiscovery(client, nil)
	releases, uncovered, _, err := discovery.DiscoverReleases(context.Background(), "prod")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(releases) != 3 {
		t.Fatalf("expected 3 releases, got %d", len(releases))
	}

	// app-a has 1 PVC and night preset
	relA := findRelease(releases, "app-a")
	if relA == nil || len(relA.PVCs) != 1 || relA.PVCs[0] != "data-app-a" {
		t.Errorf("expected app-a with data-app-a, got %+v", relA)
	}
	if !relA.OptIn {
		t.Errorf("app-a should be opted in")
	}
	if relA.AnnotatedPreset != "night" {
		t.Errorf("expected night preset annotation, got %s", relA.AnnotatedPreset)
	}

	// app-b has 0 PVCs
	relB := findRelease(releases, "app-b")
	if relB == nil || len(relB.PVCs) != 0 {
		t.Errorf("expected app-b with 0 PVCs, got %+v", relB)
	}

	// app-c is NOT opted in
	relC := findRelease(releases, "app-c")
	if relC == nil || relC.OptIn {
		t.Errorf("expected app-c to NOT be opted in")
	}

	// Uncovered PVCs must contain manual-pvc
	if len(uncovered) != 1 || uncovered[0] != "manual-pvc" {
		t.Fatalf("expected [manual-pvc], got %v", uncovered)
	}
}

func TestDiscoverReleasesViaWorkloadVolumesAndOwnerReferences(t *testing.T) {
	client := fake.NewSimpleClientset(
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "app-d-web",
				Namespace: "prod",
				UID:       "uid-app-d-web",
				Labels: map[string]string{
					HelmManagedByLabel:       "Helm",
					HelmInstanceLabel:        "app-d",
					"pvcguardian.io/enabled": "true",
				},
			},
			Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Volumes: []corev1.Volume{{
							Name: "data",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "shared-data"},
							},
						}},
					},
				},
			},
		},
		&appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "app-b-db",
				Namespace: "prod",
				UID:       "uid-app-b-db",
				Labels: map[string]string{
					HelmManagedByLabel:       "Helm",
					HelmInstanceLabel:        "app-b",
					"pvcguardian.io/enabled": "true",
				},
			},
			Spec: appsv1.StatefulSetSpec{
				VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{
					ObjectMeta: metav1.ObjectMeta{Name: "data"},
				}},
			},
		},
		// Referenced by app-d's pod template but without the instance label.
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "shared-data", Namespace: "prod"},
		},
		// Generated from app-b's volumeClaimTemplate, without the instance label.
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "data-app-b-db-0", Namespace: "prod"},
		},
		// Owned by app-d's Deployment, without the instance label.
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "owned-pvc",
				Namespace: "prod",
				OwnerReferences: []metav1.OwnerReference{{
					Kind: "Deployment",
					Name: "app-d-web",
					UID:  "uid-app-d-web",
				}},
			},
		},
		// Unrelated PVC with a similar StatefulSet-like name.
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "data-app-b-db-cache", Namespace: "prod"},
		},
		// Explicitly associated with app-b through the release label.
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "labeled-orphan",
				Namespace: "prod",
				Labels:    map[string]string{"pvcguardian.io/release": "app-b"},
			},
		},
		// Release label pointing to a release that does not exist.
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "bad-ref-pvc",
				Namespace: "prod",
				Labels:    map[string]string{"pvcguardian.io/release": "ghost"},
			},
		},
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "manual-pvc", Namespace: "prod"},
		},
	)

	discovery := NewDiscovery(client, nil)
	releases, uncovered, _, err := discovery.DiscoverReleases(context.Background(), "prod")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	relB := findRelease(releases, "app-b")
	wantB := []string{"data-app-b-db-0", "labeled-orphan"}
	if relB == nil || len(relB.PVCs) != len(wantB) {
		t.Fatalf("expected app-b with %v, got %+v", wantB, relB)
	}
	for i := range wantB {
		if relB.PVCs[i] != wantB[i] {
			t.Errorf("expected app-b with %v, got %v", wantB, relB.PVCs)
		}
	}

	relD := findRelease(releases, "app-d")
	if relD == nil {
		t.Fatalf("expected app-d release")
	}
	want := []string{"owned-pvc", "shared-data"}
	if len(relD.PVCs) != len(want) {
		t.Fatalf("expected app-d PVCs %v, got %v", want, relD.PVCs)
	}
	for i := range want {
		if relD.PVCs[i] != want[i] {
			t.Errorf("expected app-d PVCs %v, got %v", want, relD.PVCs)
		}
	}

	wantUncovered := []string{"bad-ref-pvc", "data-app-b-db-cache", "manual-pvc"}
	if len(uncovered) != len(wantUncovered) {
		t.Fatalf("expected uncovered %v, got %v", wantUncovered, uncovered)
	}
	for i := range wantUncovered {
		if uncovered[i] != wantUncovered[i] {
			t.Errorf("expected uncovered %v, got %v", wantUncovered, uncovered)
		}
	}
}

func TestDiscoverReleasesSelectorUnit(t *testing.T) {
	client := fake.NewSimpleClientset(
		// Non-Helm workload declaring its own backup scope.
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "coroot-operator",
				Namespace: "prod",
				Labels: map[string]string{
					"app.kubernetes.io/part-of": "coroot",
				},
				Annotations: map[string]string{
					SelectorAnnotation: "app.kubernetes.io/part-of=coroot",
					PresetAnnotation:   "night",
				},
			},
		},
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "coroot-clickhouse-0",
				Namespace: "prod",
				Labels: map[string]string{
					"app.kubernetes.io/part-of":   "coroot",
					"app.kubernetes.io/component": "clickhouse",
				},
			},
		},
		// Explicitly attached despite not matching the selector.
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "explicit-coroot",
				Namespace: "prod",
				Labels:    map[string]string{ReleaseLabel: "coroot-operator"},
			},
		},
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "unrelated",
				Namespace: "prod",
				Labels:    map[string]string{"app.kubernetes.io/part-of": "other"},
			},
		},
	)

	releases, uncovered, _, err := NewDiscovery(client, nil).DiscoverReleases(context.Background(), "prod")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(releases) != 1 {
		t.Fatalf("expected 1 release, got %d: %+v", len(releases), releases)
	}

	rel := findRelease(releases, "coroot-operator")
	if rel == nil {
		t.Fatalf("expected coroot-operator release, got %+v", releases)
	}
	if !rel.OptIn {
		t.Errorf("expected a selector-based unit to be opted in")
	}
	if got := rel.Selector["app.kubernetes.io/part-of"]; got != "coroot" {
		t.Errorf("expected selector part-of=coroot, got %q", got)
	}
	if rel.AnnotatedPreset != "night" {
		t.Errorf("expected night preset, got %q", rel.AnnotatedPreset)
	}

	wantPVCs := []string{"coroot-clickhouse-0", "explicit-coroot"}
	if len(rel.PVCs) != len(wantPVCs) {
		t.Fatalf("expected PVCs %v, got %v", wantPVCs, rel.PVCs)
	}
	for i := range wantPVCs {
		if rel.PVCs[i] != wantPVCs[i] {
			t.Errorf("expected PVCs %v, got %v", wantPVCs, rel.PVCs)
		}
	}

	if len(uncovered) != 1 || uncovered[0] != "unrelated" {
		t.Errorf("expected uncovered [unrelated], got %v", uncovered)
	}
}

func TestDiscoverReleasesSelectorErrors(t *testing.T) {
	client := fake.NewSimpleClientset(
		// Invalid selector: the workload is ignored.
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "invalid-selector",
				Namespace: "prod",
				Annotations: map[string]string{
					SelectorAnnotation: "app",
				},
			},
		},
		// Two workloads share a unit name but declare different selectors.
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "conflict",
				Namespace: "prod",
				Labels:    map[string]string{"app.kubernetes.io/part-of": "a"},
				Annotations: map[string]string{
					SelectorAnnotation: "app.kubernetes.io/part-of=a",
				},
			},
		},
		&appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "conflict",
				Namespace: "prod",
				Labels:    map[string]string{"app.kubernetes.io/part-of": "b"},
				Annotations: map[string]string{
					SelectorAnnotation: "app.kubernetes.io/part-of=b",
				},
			},
		},
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "conflict-pvc",
				Namespace: "prod",
				Labels:    map[string]string{"app.kubernetes.io/part-of": "a"},
			},
		},
	)

	releases, uncovered, conflicts, err := NewDiscovery(client, nil).DiscoverReleases(context.Background(), "prod")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if conflicts != 1 {
		t.Errorf("expected 1 conflicting unit, got %d", conflicts)
	}

	if len(releases) != 0 {
		t.Fatalf("expected no releases, got %+v", releases)
	}
	if len(uncovered) != 1 || uncovered[0] != "conflict-pvc" {
		t.Errorf("expected uncovered [conflict-pvc], got %v", uncovered)
	}
}

func TestDiscoverReleasesDetectsPresetConflict(t *testing.T) {
	client := fake.NewSimpleClientset(
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "web",
				Namespace: "prod",
				Labels: map[string]string{
					HelmManagedByLabel: HelmManagedByValue,
					HelmInstanceLabel:  "app-x",
					OptInLabel:         "true",
				},
				Annotations: map[string]string{
					PresetAnnotation: "day",
				},
			},
		},
		&appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "db",
				Namespace: "prod",
				Labels: map[string]string{
					HelmManagedByLabel: HelmManagedByValue,
					HelmInstanceLabel:  "app-x",
					OptInLabel:         "true",
				},
				Annotations: map[string]string{
					PresetAnnotation: "night",
				},
			},
		},
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "data-db",
				Namespace: "prod",
				Labels: map[string]string{
					HelmInstanceLabel: "app-x",
				},
			},
		},
	)

	releases, uncovered, conflicts, err := NewDiscovery(client, nil).DiscoverReleases(context.Background(), "prod")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if conflicts != 1 {
		t.Errorf("expected 1 conflict for preset mismatch, got %d", conflicts)
	}
	if len(releases) != 0 {
		t.Fatalf("expected unit app-x to be skipped due to conflict, got %d releases", len(releases))
	}
	if len(uncovered) != 1 || uncovered[0] != "data-db" {
		t.Errorf("expected data-db in uncovered PVCs, got %v", uncovered)
	}
}

func TestDiscoverReleasesDetectsOptInConflict(t *testing.T) {
	client := fake.NewSimpleClientset(
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "web",
				Namespace: "prod",
				Labels: map[string]string{
					HelmManagedByLabel: HelmManagedByValue,
					HelmInstanceLabel:  "app-y",
					OptInLabel:         "true",
				},
			},
		},
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "worker",
				Namespace: "prod",
				Labels: map[string]string{
					HelmManagedByLabel: HelmManagedByValue,
					HelmInstanceLabel:  "app-y",
					OptInLabel:         "false",
				},
			},
		},
	)

	releases, _, conflicts, err := NewDiscovery(client, nil).DiscoverReleases(context.Background(), "prod")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if conflicts != 1 {
		t.Errorf("expected 1 conflict for opt-in mismatch, got %d", conflicts)
	}
	if len(releases) != 0 {
		t.Fatalf("expected unit app-y to be skipped due to conflict, got %d releases", len(releases))
	}
}

func findRelease(releases []DiscoveredRelease, name string) *DiscoveredRelease {
	for i := range releases {
		if releases[i].Name == name {
			return &releases[i]
		}
	}
	return nil
}
