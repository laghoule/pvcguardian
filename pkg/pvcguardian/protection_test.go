// SPDX-License-Identifier: GPL-3.0-only
package pvcguardian

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"

	"github.com/laghoule/pvcguardian/pkg/config"
	"github.com/laghoule/pvcguardian/pkg/k8s"
)

func appWithPVC(name, instance string, extra map[string]string) []runtime.Object {
	labels := map[string]string{
		k8s.HelmManagedByLabel: k8s.HelmManagedByValue,
		k8s.HelmInstanceLabel:  instance,
		k8s.OptInLabel:         "true",
	}
	for k, v := range extra {
		labels[k] = v
	}
	return []runtime.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "prod", Labels: labels}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
			Name: name + "-data", Namespace: "prod",
			Labels: map[string]string{k8s.HelmInstanceLabel: instance},
		}},
	}
}

func TestConflictingUnitKeepsExistingSchedule(t *testing.T) {
	kubeClient := fake.NewSimpleClientset(append(
		[]runtime.Object{&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "prod"}}},
		appWithPVC("web", "app-a", nil)...)...)

	cfg := config.DefaultConfig()
	veleroClient := k8s.NewFakeVeleroClient()
	reconciler := NewReconciler(&cfg, "", k8s.NewDiscovery(kubeClient, nil), veleroClient, nil, nil, nil)
	reconciler.startTime = time.Now().Add(-time.Hour)

	if _, err := reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("first reconcile failed: %v", err)
	}
	key := "velero/" + ScheduleName("prod", "app-a")
	if veleroClient.Schedules[key] == nil {
		t.Fatalf("expected schedule %s to be created", key)
	}

	// A second workload of the same unit now carries a conflicting preset.
	conflicting := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Name: "db", Namespace: "prod",
		Labels: map[string]string{
			k8s.HelmManagedByLabel: k8s.HelmManagedByValue,
			k8s.HelmInstanceLabel:  "app-a",
			k8s.OptInLabel:         "true",
		},
		Annotations: map[string]string{k8s.PresetAnnotation: "night"},
	}}
	if _, err := kubeClient.AppsV1().StatefulSets("prod").Create(context.Background(), conflicting, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating conflicting statefulset: %v", err)
	}
	deployment, _ := kubeClient.AppsV1().Deployments("prod").Get(context.Background(), "web", metav1.GetOptions{})
	deployment.Annotations = map[string]string{k8s.PresetAnnotation: "day"}
	if _, err := kubeClient.AppsV1().Deployments("prod").Update(context.Background(), deployment, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("updating deployment: %v", err)
	}

	res, _ := reconciler.ReconcileOnce(context.Background())
	if res.ConflictingUnits != 1 {
		t.Fatalf("expected 1 conflicting unit, got %d", res.ConflictingUnits)
	}
	if res.DeletedSchedules != 0 {
		t.Errorf("conflicting unit must not delete its schedule, deleted %d", res.DeletedSchedules)
	}
	if veleroClient.Schedules[key] == nil {
		t.Errorf("schedule %s was deleted while its unit is in conflict", key)
	}
}

func TestUpdatePreservesOperatorFields(t *testing.T) {
	kubeClient := fake.NewSimpleClientset(append(
		[]runtime.Object{&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "prod"}}},
		appWithPVC("web", "app-a", nil)...)...)

	cfg := config.DefaultConfig()
	veleroClient := k8s.NewFakeVeleroClient()
	reconciler := NewReconciler(&cfg, "", k8s.NewDiscovery(kubeClient, nil), veleroClient, nil, nil, nil)
	if _, err := reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("first reconcile failed: %v", err)
	}

	key := "velero/" + ScheduleName("prod", "app-a")
	existing := veleroClient.Schedules[key]
	existing.Spec.Paused = true
	existing.Labels["operator"] = "manual"
	existing.Annotations["note"] = "kept"

	cfg.Schedules.Retention = "10d"
	if _, err := reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("second reconcile failed: %v", err)
	}

	updated := veleroClient.Schedules[key]
	if !updated.Spec.Paused {
		t.Errorf("spec.paused set by the operator was reset")
	}
	if updated.Labels["operator"] != "manual" {
		t.Errorf("operator label was removed")
	}
	if updated.Annotations["note"] != "kept" {
		t.Errorf("operator annotation was removed")
	}
	if updated.Spec.Template.TTL.Duration != 10*24*time.Hour {
		t.Errorf("managed TTL not updated, got %v", updated.Spec.Template.TTL.Duration)
	}
}

func TestInstanceIDIsolatesSchedules(t *testing.T) {
	kubeClient := fake.NewSimpleClientset(append(
		[]runtime.Object{&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "prod"}}},
		appWithPVC("web", "app-a", nil)...)...)

	veleroClient := k8s.NewFakeVeleroClient()
	other := &velerov1.Schedule{ObjectMeta: metav1.ObjectMeta{
		Name: "other-owned", Namespace: "velero",
		Labels: map[string]string{
			ManagedByLabel: ManagedByValue,
			InstanceLabel:  "team-b",
		},
		Annotations: map[string]string{
			SourceNamespaceAnno: "prod",
			SourceReleaseAnno:   "app-a",
		},
	}}
	veleroClient.Schedules["velero/other-owned"] = other

	cfg := config.DefaultConfig()
	cfg.Velero.InstanceID = "team-a"
	reconciler := NewReconciler(&cfg, "", k8s.NewDiscovery(kubeClient, nil), veleroClient, nil, nil, nil)
	res, err := reconciler.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	if res.DeletedSchedules != 0 || veleroClient.Schedules["velero/other-owned"] == nil {
		t.Errorf("installation team-a must not touch schedules owned by team-b")
	}
	created := veleroClient.Schedules["velero/"+ScheduleName("prod", "app-a")]
	if created == nil || created.Labels[InstanceLabel] != "team-a" {
		t.Errorf("expected schedule labelled with instance team-a, got %+v", created)
	}
}
