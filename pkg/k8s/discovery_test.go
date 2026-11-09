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
	releases, uncovered, err := discovery.DiscoverReleases(context.Background(), "prod", "pvcguardian.io/enabled", "pvcguardian.io/preset")
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

func findRelease(releases []DiscoveredRelease, name string) *DiscoveredRelease {
	for i := range releases {
		if releases[i].Name == name {
			return &releases[i]
		}
	}
	return nil
}
