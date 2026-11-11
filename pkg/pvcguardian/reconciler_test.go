// SPDX-License-Identifier: GPL-3.0-only
package pvcguardian

import (
	"context"
	"fmt"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/laghoule/pvcguardian/pkg/config"
	"github.com/laghoule/pvcguardian/pkg/k8s"
)

func TestReconcileOnceCreatesAndDeletesOrphans(t *testing.T) {
	// 1. Setup K8s state with two releases:
	// 'app-with-pvc' is opted-in with label and has PVC -> schedule created
	// 'app-no-pvc' is opted-in but has no PVC -> skipped (requirePVC=true)
	// 'app-not-opted-in' has PVC but no opt-in label -> skipped
	kubeClient := fake.NewSimpleClientset(
		&corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: "prod",
			},
		},
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "web-deploy",
				Namespace: "prod",
				Labels: map[string]string{
					k8s.HelmManagedByLabel:   "Helm",
					k8s.HelmInstanceLabel:    "app-with-pvc",
					"pvcguardian.io/enabled": "true",
				},
			},
		},
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "web-pvc",
				Namespace: "prod",
				Labels: map[string]string{
					k8s.HelmInstanceLabel: "app-with-pvc",
				},
			},
		},
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "stateless-deploy",
				Namespace: "prod",
				Labels: map[string]string{
					k8s.HelmManagedByLabel:   "Helm",
					k8s.HelmInstanceLabel:    "app-no-pvc",
					"pvcguardian.io/enabled": "true",
				},
			},
		},
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "legacy-deploy",
				Namespace: "prod",
				Labels: map[string]string{
					k8s.HelmManagedByLabel: "Helm",
					k8s.HelmInstanceLabel:  "app-not-opted-in",
				},
			},
		},
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "legacy-pvc",
				Namespace: "prod",
				Labels: map[string]string{
					k8s.HelmInstanceLabel: "app-not-opted-in",
				},
			},
		},
	)

	cfg := config.DefaultConfig()
	cfg.Discovery.RequirePVC = true
	cfg.Velero.Namespace = "velero"

	discovery := k8s.NewDiscovery(kubeClient, nil)
	veleroClient := k8s.NewFakeVeleroClient()
	metrics := NewMetrics()

	// A schedule created by another mechanism: it must be counted as
	// unmanaged and left untouched by reconciliation.
	if _, err := veleroClient.CreateSchedule(context.Background(), &velerov1.Schedule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "manual-nightly",
			Namespace: "velero",
			Labels: map[string]string{
				k8s.HelmManagedByLabel: "manual",
			},
		},
	}); err != nil {
		t.Fatalf("failed to seed unmanaged schedule: %v", err)
	}

	reconciler := NewReconciler(&cfg, "", discovery, veleroClient, nil, metrics, nil)

	// FIRST CYCLE
	res, err := reconciler.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("first reconcile failed: %v", err)
	}

	if res.CreatedSchedules != 1 {
		t.Errorf("expected 1 created schedule, got %d", res.CreatedSchedules)
	}
	if res.SkippedNoPVC != 1 {
		t.Errorf("expected 1 skipped release without PVC, got %d", res.SkippedNoPVC)
	}
	if res.SkippedOptOut != 1 {
		t.Errorf("expected 1 skipped release not opted in, got %d", res.SkippedOptOut)
	}
	if res.SkippedTotal() != 2 {
		t.Errorf("expected 2 skipped releases in total, got %d", res.SkippedTotal())
	}
	if res.SchedulesManaged != 1 {
		t.Errorf("expected 1 managed schedule, got %d", res.SchedulesManaged)
	}
	if got := gaugeValue(t, metrics.SchedulesManaged); got != 1 {
		t.Errorf("expected schedules_managed gauge 1, got %v", got)
	}
	if res.SchedulesUnmanaged != 1 {
		t.Errorf("expected 1 unmanaged schedule, got %d", res.SchedulesUnmanaged)
	}
	if got := gaugeValue(t, metrics.SchedulesUnmanaged); got != 1 {
		t.Errorf("expected schedules_unmanaged gauge 1, got %v", got)
	}
	if got := gaugeVecValue(t, metrics.ReleasesSkipped, "opt_out"); got != 1 {
		t.Errorf("expected releases_skipped{reason=opt_out} gauge 1, got %v", got)
	}
	if got := gaugeVecValue(t, metrics.ReleasesSkipped, "no_pvc"); got != 1 {
		t.Errorf("expected releases_skipped{reason=no_pvc} gauge 1, got %v", got)
	}
	if len(veleroClient.Schedules) != 2 {
		t.Fatalf("expected 2 schedules (1 managed + 1 unmanaged) in velero client, got %d", len(veleroClient.Schedules))
	}

	sch := veleroClient.Schedules["velero/pvcguardian-prod-app-with-pvc"]
	if sch == nil {
		t.Fatalf("expected schedule velero/pvcguardian-prod-app-with-pvc to exist")
	}

	// SECOND CYCLE: idempotent pass (no creations, no updates, no deletions)
	res2, err := reconciler.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("second reconcile failed: %v", err)
	}
	if res2.CreatedSchedules != 0 || res2.UpdatedSchedules != 0 || res2.DeletedSchedules != 0 {
		t.Errorf("expected 0 modifications on second run, got %+v", res2)
	}
	if res2.SchedulesManaged != 1 {
		t.Errorf("expected 1 managed schedule on second run, got %d", res2.SchedulesManaged)
	}

	// RESTART: a fresh reconciler must count the schedule created earlier,
	// instead of starting its gauge at zero.
	restarted := NewReconciler(&cfg, "", discovery, veleroClient, nil, metrics, nil)
	resRestart, err := restarted.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("restart reconcile failed: %v", err)
	}
	if resRestart.SchedulesManaged != 1 {
		t.Errorf("expected 1 managed schedule after restart, got %d", resRestart.SchedulesManaged)
	}
	if resRestart.CreatedSchedules != 0 || resRestart.UpdatedSchedules != 0 || resRestart.DeletedSchedules != 0 {
		t.Errorf("expected no modifications after restart, got %+v", resRestart)
	}
	if got := gaugeValue(t, metrics.SchedulesManaged); got != 1 {
		t.Errorf("expected schedules_managed gauge 1 after restart, got %v", got)
	}

	// THIRD CYCLE: remove the PVC -> release now has no PVCs -> schedule should be deleted as orphaned
	err = kubeClient.CoreV1().PersistentVolumeClaims("prod").Delete(context.Background(), "web-pvc", metav1.DeleteOptions{})
	if err != nil {
		t.Fatalf("failed to delete PVC: %v", err)
	}

	res3, err := reconciler.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("third reconcile failed: %v", err)
	}
	if res3.DeletedSchedules != 1 {
		t.Errorf("expected 1 deleted schedule, got %d", res3.DeletedSchedules)
	}
	if res3.SchedulesManaged != 0 {
		t.Errorf("expected 0 managed schedules after orphan deletion, got %d", res3.SchedulesManaged)
	}
	if got := gaugeValue(t, metrics.SchedulesManaged); got != 0 {
		t.Errorf("expected schedules_managed gauge 0 after orphan deletion, got %v", got)
	}
	if got := gaugeVecValue(t, metrics.ReleasesSkipped, "no_pvc"); got != 2 {
		t.Errorf("expected releases_skipped{reason=no_pvc} gauge 2 after orphan deletion, got %v", got)
	}
	if got := gaugeVecValue(t, metrics.ReleasesSkipped, "opt_out"); got != 1 {
		t.Errorf("expected releases_skipped{reason=opt_out} gauge 1 after orphan deletion, got %v", got)
	}
	if res3.SchedulesUnmanaged != 1 {
		t.Errorf("expected 1 unmanaged schedule after orphan deletion, got %d", res3.SchedulesUnmanaged)
	}
	if got := gaugeValue(t, metrics.SchedulesUnmanaged); got != 1 {
		t.Errorf("expected schedules_unmanaged gauge 1 after orphan deletion, got %v", got)
	}
	if len(veleroClient.Schedules) != 1 {
		t.Errorf("expected the unmanaged schedule to remain, got %d schedules", len(veleroClient.Schedules))
	}
	if veleroClient.Schedules["velero/manual-nightly"] == nil {
		t.Errorf("expected unmanaged schedule velero/manual-nightly to be preserved")
	}
}

func TestReconcileUpdatesOnConfigChange(t *testing.T) {
	kubeClient := fake.NewSimpleClientset(
		&corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: "prod",
			},
		},
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "web-deploy",
				Namespace: "prod",
				Labels: map[string]string{
					k8s.HelmManagedByLabel:   "Helm",
					k8s.HelmInstanceLabel:    "app-a",
					"pvcguardian.io/enabled": "true",
				},
			},
		},
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "web-pvc",
				Namespace: "prod",
				Labels: map[string]string{
					k8s.HelmInstanceLabel: "app-a",
				},
			},
		},
	)

	cfg := config.DefaultConfig()
	cfg.Schedules.Retention = "5d"

	discovery := k8s.NewDiscovery(kubeClient, nil)
	veleroClient := k8s.NewFakeVeleroClient()

	reconciler := NewReconciler(&cfg, "", discovery, veleroClient, nil, nil, nil)
	_, _ = reconciler.ReconcileOnce(context.Background())

	// Change retention to 10d
	cfg.Schedules.Retention = "10d"
	res, err := reconciler.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	if res.UpdatedSchedules != 1 {
		t.Errorf("expected 1 updated schedule, got %d", res.UpdatedSchedules)
	}

	sch := veleroClient.Schedules["velero/pvcguardian-prod-app-a"]
	if sch == nil || sch.Spec.Template.TTL.Duration != 10*24*time.Hour {
		t.Errorf("expected updated TTL 10d, got %v", sch.Spec.Template.TTL.Duration)
	}
}

func TestReconcileAppliesSelectorChanges(t *testing.T) {
	ctx := context.Background()

	kubeClient := fake.NewSimpleClientset(
		&corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: "prod"},
		},
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "web",
				Namespace: "prod",
				Annotations: map[string]string{
					k8s.SelectorAnnotation: "app=foo",
				},
			},
		},
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "data",
				Namespace: "prod",
				Labels:    map[string]string{"app": "foo"},
			},
		},
	)

	cfg := config.DefaultConfig()
	cfg.Velero.Namespace = "velero"

	discovery := k8s.NewDiscovery(kubeClient, nil)
	veleroClient := k8s.NewFakeVeleroClient()
	reconciler := NewReconciler(&cfg, "", discovery, veleroClient, nil, nil, nil)

	if _, err := reconciler.ReconcileOnce(ctx); err != nil {
		t.Fatalf("initial reconcile failed: %v", err)
	}

	const scheduleKey = "velero/pvcguardian-prod-web"
	sch := veleroClient.Schedules[scheduleKey]
	if sch == nil {
		t.Fatalf("expected schedule %s to exist", scheduleKey)
	}
	if got := sch.Spec.Template.OrLabelSelectors[0].MatchLabels["app"]; got != "foo" {
		t.Fatalf("expected initial selector app=foo, got %q", got)
	}

	// Change the selector annotation and the matching PVC label.
	dep, err := kubeClient.AppsV1().Deployments("prod").Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting deployment: %v", err)
	}
	dep.Annotations[k8s.SelectorAnnotation] = "app=bar"
	if _, err := kubeClient.AppsV1().Deployments("prod").Update(ctx, dep, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("updating deployment: %v", err)
	}

	pvc, err := kubeClient.CoreV1().PersistentVolumeClaims("prod").Get(ctx, "data", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting PVC: %v", err)
	}
	pvc.Labels["app"] = "bar"
	if _, err := kubeClient.CoreV1().PersistentVolumeClaims("prod").Update(ctx, pvc, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("updating PVC: %v", err)
	}

	res, err := reconciler.ReconcileOnce(ctx)
	if err != nil {
		t.Fatalf("second reconcile failed: %v", err)
	}
	if res.UpdatedSchedules != 1 {
		t.Errorf("expected 1 updated schedule after selector change, got %d", res.UpdatedSchedules)
	}

	sch = veleroClient.Schedules[scheduleKey]
	if sch == nil {
		t.Fatalf("expected schedule %s to still exist", scheduleKey)
	}
	if got := sch.Spec.Template.OrLabelSelectors[0].MatchLabels["app"]; got != "bar" {
		t.Errorf("expected updated selector app=bar, got %q", got)
	}
}

func TestNeedsUpdate(t *testing.T) {
	boolPtr := func(b bool) *bool { return &b }

	desired := func() *velerov1.Schedule {
		return &velerov1.Schedule{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{PresetAnno: "night"},
			},
			Spec: velerov1.ScheduleSpec{
				Schedule: "0 2 * * *",
				Template: velerov1.BackupSpec{
					IncludedNamespaces: []string{"prod"},
					OrLabelSelectors: []*metav1.LabelSelector{
						{MatchLabels: map[string]string{"app.kubernetes.io/part-of": "coroot"}},
						{MatchLabels: map[string]string{k8s.ReleaseLabel: "coroot"}},
					},
					TTL:                      metav1.Duration{Duration: 5 * 24 * time.Hour},
					StorageLocation:          "default",
					DefaultVolumesToFsBackup: boolPtr(true),
					SnapshotVolumes:          boolPtr(false),
				},
			},
		}
	}

	cases := []struct {
		name   string
		mutate func(*velerov1.Schedule)
		want   bool
	}{
		{
			name:   "identical",
			mutate: func(*velerov1.Schedule) {},
			want:   false,
		},
		{
			name:   "cron changed",
			mutate: func(s *velerov1.Schedule) { s.Spec.Schedule = "0 3 * * *" },
			want:   true,
		},
		{
			name:   "TTL changed",
			mutate: func(s *velerov1.Schedule) { s.Spec.Template.TTL = metav1.Duration{Duration: 24 * time.Hour} },
			want:   true,
		},
		{
			name:   "storage location changed",
			mutate: func(s *velerov1.Schedule) { s.Spec.Template.StorageLocation = "secondary" },
			want:   true,
		},
		{
			name:   "preset changed",
			mutate: func(s *velerov1.Schedule) { s.Annotations[PresetAnno] = "day" },
			want:   true,
		},
		{
			name:   "included namespaces changed",
			mutate: func(s *velerov1.Schedule) { s.Spec.Template.IncludedNamespaces = []string{"other"} },
			want:   true,
		},
		{
			name: "label selector changed",
			mutate: func(s *velerov1.Schedule) {
				s.Spec.Template.LabelSelector = &metav1.LabelSelector{
					MatchLabels: map[string]string{"app": "other"},
				}
			},
			want: true,
		},
		{
			name: "or label selector changed",
			mutate: func(s *velerov1.Schedule) {
				s.Spec.Template.OrLabelSelectors[0].MatchLabels["app.kubernetes.io/part-of"] = "other"
			},
			want: true,
		},
		{
			name: "default volumes to fs backup changed",
			mutate: func(s *velerov1.Schedule) {
				s.Spec.Template.DefaultVolumesToFsBackup = boolPtr(false)
			},
			want: true,
		},
		{
			name: "snapshot volumes changed",
			mutate: func(s *velerov1.Schedule) {
				s.Spec.Template.SnapshotVolumes = boolPtr(true)
			},
			want: true,
		},
	}

	reconciler := &Reconciler{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			existing := desired()
			tc.mutate(existing)
			if got := reconciler.needsUpdate(existing, desired()); got != tc.want {
				t.Errorf("needsUpdate() = %v, want %v", got, tc.want)
			}
		})
	}
}

// gaugeValue reads the current value of a gauge without pulling in the
// prometheus testutil package.
func gaugeValue(t *testing.T, g prometheus.Gauge) float64 {
	t.Helper()

	reg := prometheus.NewRegistry()
	if err := reg.Register(g); err != nil {
		t.Fatalf("registering gauge: %v", err)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gathering metrics: %v", err)
	}
	if len(families) != 1 || len(families[0].Metric) != 1 {
		t.Fatalf("unexpected metric families: %+v", families)
	}
	return families[0].Metric[0].GetGauge().GetValue()
}

// gaugeVecValue reads the value of one labelled series from a GaugeVec.
func gaugeVecValue(t *testing.T, gv *prometheus.GaugeVec, reason string) float64 {
	t.Helper()

	reg := prometheus.NewRegistry()
	if err := reg.Register(gv); err != nil {
		t.Fatalf("registering gauge vec: %v", err)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gathering metrics: %v", err)
	}
	if len(families) != 1 {
		t.Fatalf("unexpected metric families: %+v", families)
	}
	for _, metric := range families[0].Metric {
		for _, label := range metric.Label {
			if label.GetName() == "reason" && label.GetValue() == reason {
				return metric.GetGauge().GetValue()
			}
		}
	}
	t.Fatalf("series with reason=%q not found", reason)
	return 0
}

func TestReconcilePreservesSchedulesOnDiscoveryFailure(t *testing.T) {
	ctx := context.Background()

	kubeClient := fake.NewSimpleClientset(
		&corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: "prod"},
		},
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "web",
				Namespace: "prod",
				Labels: map[string]string{
					k8s.HelmManagedByLabel: k8s.HelmManagedByValue,
					k8s.HelmInstanceLabel:  "web-release",
					k8s.OptInLabel:         "true",
				},
			},
			Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Volumes: []corev1.Volume{
							{
								Name: "data",
								VolumeSource: corev1.VolumeSource{
									PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
										ClaimName: "web-data",
									},
								},
							},
						},
					},
				},
			},
		},
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "web-data",
				Namespace: "prod",
				Labels: map[string]string{
					k8s.HelmInstanceLabel: "web-release",
				},
			},
		},
	)

	cfg := config.DefaultConfig()
	cfg.Velero.Namespace = "velero"

	discovery := k8s.NewDiscovery(kubeClient, nil)
	veleroClient := k8s.NewFakeVeleroClient()
	reconciler := NewReconciler(&cfg, "", discovery, veleroClient, nil, nil, nil)

	// First pass: schedule created
	res, err := reconciler.ReconcileOnce(ctx)
	if err != nil {
		t.Fatalf("initial reconcile failed: %v", err)
	}
	if res.CreatedSchedules != 1 {
		t.Fatalf("expected 1 created schedule, got %d", res.CreatedSchedules)
	}
	const scheduleKey = "velero/pvcguardian-prod-web-release"
	if veleroClient.Schedules[scheduleKey] == nil {
		t.Fatalf("expected schedule %s to exist", scheduleKey)
	}

	// Now simulate API failure on listing deployments by deleting namespace or prepending reactor
	// If deployment listing fails in prod namespace, DiscoverReleases will fail.
	kubeClient.PrependReactor("list", "deployments", func(action clienttesting.Action) (handled bool, ret runtime.Object, err error) {
		return true, nil, fmt.Errorf("simulated API server timeout")
	})

	res, err = reconciler.ReconcileOnce(ctx)
	if err == nil {
		t.Fatalf("expected error from ReconcileOnce on discovery failure, got nil")
	}

	// Crucial check: schedule MUST NOT be deleted
	if veleroClient.Schedules[scheduleKey] == nil {
		t.Fatalf("schedule %s was deleted despite discovery failure in its namespace!", scheduleKey)
	}
	if res.DeletedSchedules != 0 {
		t.Errorf("expected 0 deleted schedules, got %d", res.DeletedSchedules)
	}
}
