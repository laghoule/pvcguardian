// SPDX-License-Identifier: GPL-3.0-only
package pvcguardian

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/laghoule/pvcguardian/pkg/config"
	"github.com/laghoule/pvcguardian/pkg/k8s"
)

func TestLeaderElectionConstantsAreCoherent(t *testing.T) {
	// The leader must have several renewal opportunities before giving up, and
	// followers must wait longer than the renewal deadline before taking over.
	if LeaderElectionLeaseDuration <= LeaderElectionRenewDeadline {
		t.Errorf("lease duration %s must be greater than renew deadline %s",
			LeaderElectionLeaseDuration, LeaderElectionRenewDeadline)
	}
	if LeaderElectionRenewDeadline <= LeaderElectionRetryPeriod {
		t.Errorf("renew deadline %s must be greater than retry period %s",
			LeaderElectionRenewDeadline, LeaderElectionRetryPeriod)
	}
	if LeaderElectionRenewDeadline < 3*LeaderElectionRetryPeriod {
		t.Errorf("renew deadline %s must allow at least 3 retries of %s",
			LeaderElectionRenewDeadline, LeaderElectionRetryPeriod)
	}
}

func TestLeaderLeaseLockUsesDeploymentIdentity(t *testing.T) {
	client := fake.NewSimpleClientset()
	lock := newLeaderLeaseLock(client.CoordinationV1(), "pvcguardian-system", "my-release-pvcguardian", "pod-1")

	if lock.LeaseMeta.Name != "my-release-pvcguardian" {
		t.Errorf("expected lease name my-release-pvcguardian, got %q", lock.LeaseMeta.Name)
	}
	if lock.LeaseMeta.Namespace != "pvcguardian-system" {
		t.Errorf("expected lease namespace pvcguardian-system, got %q", lock.LeaseMeta.Namespace)
	}
	if lock.LockConfig.Identity != "pod-1" {
		t.Errorf("expected identity pod-1, got %q", lock.LockConfig.Identity)
	}
	if lock.Client == nil {
		t.Error("expected a lease client")
	}
}

func TestLeaderLeaseNameFromDeployment(t *testing.T) {
	t.Setenv(leaderLeaseNameEnv, "my-release-pvcguardian")

	name, err := leaderLeaseName()
	if err != nil {
		t.Fatalf("leader lease name: %v", err)
	}
	if name != "my-release-pvcguardian" {
		t.Errorf("expected lease name my-release-pvcguardian, got %q", name)
	}
}

func TestLeaderLeaseNameFallsBackToDefault(t *testing.T) {
	t.Setenv(leaderLeaseNameEnv, "")

	name, err := leaderLeaseName()
	if err != nil {
		t.Fatalf("leader lease name: %v", err)
	}
	if name != DefaultLeaderElectionLeaseName {
		t.Errorf("expected lease name %q, got %q", DefaultLeaderElectionLeaseName, name)
	}
}

func TestLeaderLeaseNameRejectsInvalidValue(t *testing.T) {
	t.Setenv(leaderLeaseNameEnv, "Not_Valid")

	if _, err := leaderLeaseName(); err == nil {
		t.Error("expected an invalid lease name to be rejected")
	}
}

func TestLeaderLeaseNamespaceFromDownwardAPI(t *testing.T) {
	t.Setenv(leaderNamespaceEnv, "pvcguardian-system")

	namespace, err := leaderLeaseNamespace()
	if err != nil {
		t.Fatalf("leader lease namespace: %v", err)
	}
	if namespace != "pvcguardian-system" {
		t.Errorf("expected lease namespace pvcguardian-system, got %q", namespace)
	}
}

func TestLeaderLeaseNamespaceRejectsInvalidValue(t *testing.T) {
	t.Setenv(leaderNamespaceEnv, "Not_Valid")

	if _, err := leaderLeaseNamespace(); err == nil {
		t.Error("expected an invalid lease namespace to be rejected")
	}
}

func TestLeaderIdentityUsesHostname(t *testing.T) {
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatalf("hostname: %v", err)
	}

	identity, err := leaderIdentity()
	if err != nil {
		t.Fatalf("leader identity: %v", err)
	}
	if identity != hostname {
		t.Errorf("expected identity %q, got %q", hostname, identity)
	}
}

func TestRunWithLeaderElectionAcquiresAndReleasesLease(t *testing.T) {
	t.Setenv(leaderLeaseNameEnv, "my-release-pvcguardian")
	t.Setenv(leaderNamespaceEnv, "pvcguardian-system")

	kubeClient := fake.NewSimpleClientset()

	cfg := config.DefaultConfig()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reconciler := NewReconciler(&cfg, "", k8s.NewDiscovery(kubeClient, logger), k8s.NewFakeVeleroClient(), nil, NewMetrics(), logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- reconciler.RunWithLeaderElection(ctx, kubeClient)
	}()

	waitFor(t, 5*time.Second, func() bool { return reconciler.IsLeader() })

	hostname, err := os.Hostname()
	if err != nil {
		t.Fatalf("hostname: %v", err)
	}
	lease, err := kubeClient.CoordinationV1().Leases("pvcguardian-system").Get(ctx, "my-release-pvcguardian", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting lease: %v", err)
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != hostname {
		t.Errorf("expected holder identity %q, got %v", hostname, lease.Spec.HolderIdentity)
	}

	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("expected graceful shutdown, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunWithLeaderElection did not return after cancellation")
	}

	if reconciler.IsLeader() {
		t.Error("expected the replica to stop being leader after shutdown")
	}

	lease, err = kubeClient.CoordinationV1().Leases("pvcguardian-system").Get(context.Background(), "my-release-pvcguardian", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting released lease: %v", err)
	}
	if lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity != "" {
		t.Errorf("expected the lease to be released, holder is %q", *lease.Spec.HolderIdentity)
	}
}

func TestReconcilerIsNotLeaderBeforeElection(t *testing.T) {
	cfg := config.DefaultConfig()
	reconciler := NewReconciler(&cfg, "", nil, nil, nil, nil, nil)

	if reconciler.IsLeader() {
		t.Error("expected a new reconciler not to be leader")
	}

	// The leader flag must also work without a metrics registry.
	reconciler.setLeader(true)
	if !reconciler.IsLeader() {
		t.Error("expected reconciler to become leader")
	}
	reconciler.setLeader(false)
	if reconciler.IsLeader() {
		t.Error("expected reconciler to stop being leader")
	}
}

func TestSetLeaderUpdatesMetric(t *testing.T) {
	m := NewMetrics()
	m.SetLeader(true)
	if got := gaugeValue(t, m.Leader); got != 1 {
		t.Errorf("expected leader gauge 1, got %v", got)
	}
	m.SetLeader(false)
	if got := gaugeValue(t, m.Leader); got != 0 {
		t.Errorf("expected leader gauge 0, got %v", got)
	}
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}
