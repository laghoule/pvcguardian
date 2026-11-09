// SPDX-License-Identifier: GPL-3.0-only
package pvcguardian

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

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
	)

	cfg := config.DefaultConfig()
	cfg.Discovery.RequirePVC = true
	cfg.Velero.Namespace = "velero"

	discovery := k8s.NewDiscovery(kubeClient, nil)
	veleroClient := k8s.NewFakeVeleroClient()

	reconciler := NewReconciler(&cfg, "", discovery, veleroClient, nil, nil)

	// FIRST CYCLE
	res, err := reconciler.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("first reconcile failed: %v", err)
	}

	if res.CreatedSchedules != 1 {
		t.Errorf("expected 1 created schedule, got %d", res.CreatedSchedules)
	}
	if res.SkippedReleases != 1 {
		t.Errorf("expected 1 skipped release (stateless), got %d", res.SkippedReleases)
	}
	if len(veleroClient.Schedules) != 1 {
		t.Fatalf("expected 1 schedule in velero client, got %d", len(veleroClient.Schedules))
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
	if len(veleroClient.Schedules) != 0 {
		t.Errorf("expected 0 schedules remaining, got %d", len(veleroClient.Schedules))
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

	reconciler := NewReconciler(&cfg, "", discovery, veleroClient, nil, nil)
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
