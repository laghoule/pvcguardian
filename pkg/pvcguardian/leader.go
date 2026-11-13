// SPDX-License-Identifier: GPL-3.0-only
package pvcguardian

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	coordinationv1client "k8s.io/client-go/kubernetes/typed/coordination/v1"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// Leader election timing and identity settings are constants on purpose. They
// describe how all PVCGuardian replicas cooperate, not how the cluster should
// be backed up, so they are not exposed through the YAML configuration or Helm
// values. The lease name and namespace are not constants: they are inherited
// from the Deployment and from the namespace where PVCGuardian is installed, so
// two installations can never compete for the same lease.
const (
	// DefaultLeaderElectionLeaseName is used when the Deployment name is not
	// provided, for example when running the binary outside Helm.
	DefaultLeaderElectionLeaseName = "pvcguardian"

	// LeaderElectionLeaseDuration is how long a follower waits without
	// observing a lease renewal before trying to take over.
	LeaderElectionLeaseDuration = 15 * time.Second

	// LeaderElectionRenewDeadline is how long the leader keeps retrying to
	// renew the lease before giving up and stopping its reconciliation loop.
	LeaderElectionRenewDeadline = 10 * time.Second

	// LeaderElectionRetryPeriod is the interval between lease acquisition and
	// renewal attempts.
	LeaderElectionRetryPeriod = 2 * time.Second
)

const (
	// leaderLeaseNameEnv is set by the Helm chart to the Deployment name.
	leaderLeaseNameEnv = "PVCGUARDIAN_LEASE_NAME"
	// leaderNamespaceEnv is set from the pod's metadata.namespace.
	leaderNamespaceEnv = "POD_NAMESPACE"
	// leaderNamespaceFile is mounted in every pod and is the fallback when the
	// downward-API environment variable is absent.
	leaderNamespaceFile            = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
	defaultLeaderElectionNamespace = "default"
)

// leaderLeaseName returns the lease name derived from the Deployment name. It
// falls back to DefaultLeaderElectionLeaseName when the environment variable is
// absent (non-Helm deployments).
func leaderLeaseName() (string, error) {
	name := strings.TrimSpace(os.Getenv(leaderLeaseNameEnv))
	if name == "" {
		return DefaultLeaderElectionLeaseName, nil
	}
	if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
		return "", fmt.Errorf("invalid leader election lease name %q from %s: %s",
			name, leaderLeaseNameEnv, strings.Join(errs, "; "))
	}
	return name, nil
}

// leaderLeaseNamespace returns the namespace where PVCGuardian is installed: the
// pod namespace injected by the downward API, the mounted service account
// namespace, or "default" outside a cluster.
func leaderLeaseNamespace() (string, error) {
	if namespace := strings.TrimSpace(os.Getenv(leaderNamespaceEnv)); namespace != "" {
		if errs := validation.IsDNS1123Label(namespace); len(errs) > 0 {
			return "", fmt.Errorf("invalid leader election namespace %q from %s: %s",
				namespace, leaderNamespaceEnv, strings.Join(errs, "; "))
		}
		return namespace, nil
	}

	data, err := os.ReadFile(leaderNamespaceFile)
	if err == nil {
		namespace := strings.TrimSpace(string(data))
		if namespace != "" {
			if errs := validation.IsDNS1123Label(namespace); len(errs) > 0 {
				return "", fmt.Errorf("invalid leader election namespace %q from %s: %s",
					namespace, leaderNamespaceFile, strings.Join(errs, "; "))
			}
			return namespace, nil
		}
	}

	return defaultLeaderElectionNamespace, nil
}

// leaderIdentity returns the unique identity of this replica. Kubernetes sets
// the hostname to the pod name, which is unique within a cluster.
func leaderIdentity() (string, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("determining leader election identity: %w", err)
	}
	if hostname == "" {
		return "", errors.New("determining leader election identity: hostname is empty")
	}
	return hostname, nil
}

// newLeaderLeaseLock builds the Lease lock used for election. The lease is named
// after the Deployment and lives in the PVCGuardian installation namespace.
func newLeaderLeaseLock(client coordinationv1client.LeasesGetter, namespace, name, identity string) *resourcelock.LeaseLock {
	return &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Client: client,
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: identity,
		},
	}
}

// IsLeader reports whether this replica currently holds the leader lease.
func (r *Reconciler) IsLeader() bool {
	return r.leader.Load()
}

func (r *Reconciler) setLeader(isLeader bool) {
	r.leader.Store(isLeader)
	if r.metrics != nil {
		r.metrics.SetLeader(isLeader)
	}
}

// RunWithLeaderElection participates in leader election and only runs the
// reconciliation loop while this replica holds the lease. Followers stay alive
// and ready to take over without writing any Schedule.
//
// It returns nil when ctx is canceled, which is the graceful shutdown path.
// Losing the lease without a shutdown is reported as an error: the caller must
// exit so Kubernetes restarts the process, which then re-joins the election.
func (r *Reconciler) RunWithLeaderElection(ctx context.Context, kubeClient kubernetes.Interface) error {
	namespace, err := leaderLeaseNamespace()
	if err != nil {
		return err
	}

	leaseName, err := leaderLeaseName()
	if err != nil {
		return err
	}

	identity, err := leaderIdentity()
	if err != nil {
		return err
	}

	lock := newLeaderLeaseLock(kubeClient.CoordinationV1(), namespace, leaseName, identity)

	// The callbacks run in their own goroutine. loopStarted lets the shutdown
	// path distinguish "never led, so no loop to wait for" from "led earlier,
	// wait for the loop to observe cancellation".
	loopStarted := make(chan struct{})
	loopDone := make(chan struct{})

	callbacks := leaderelection.LeaderCallbacks{
		OnStartedLeading: func(leaderCtx context.Context) {
			close(loopStarted)
			r.setLeader(true)
			// Defers run in reverse order: clear the leader flag before
			// signalling that the loop has fully stopped.
			defer close(loopDone)
			defer r.setLeader(false)

			if err := r.RunLoop(leaderCtx); err != nil && !errors.Is(err, context.Canceled) {
				r.logger.Error("Reconciliation loop exited with an error", "error", err)
			}
		},
		OnStoppedLeading: func() {
			select {
			case <-loopStarted:
				if ctx.Err() != nil {
					r.logger.Info("Releasing leadership during shutdown")
					return
				}
				r.logger.Error("Lost leadership; this replica will no longer reconcile Schedules")
			default:
				r.logger.Debug("Leader election stopped before this replica acquired the lease")
			}
		},
		OnNewLeader: func(newLeader string) {
			if newLeader == identity {
				return
			}
			r.logger.Info("Observed another PVCGuardian leader", "leader", newLeader)
		},
	}

	elector, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock:            lock,
		LeaseDuration:   LeaderElectionLeaseDuration,
		RenewDeadline:   LeaderElectionRenewDeadline,
		RetryPeriod:     LeaderElectionRetryPeriod,
		Callbacks:       callbacks,
		ReleaseOnCancel: true,
		Name:            leaseName,
	})
	if err != nil {
		return fmt.Errorf("creating leader elector: %w", err)
	}

	r.logger.Info("Starting leader election",
		"lease", fmt.Sprintf("%s/%s", namespace, leaseName),
		"identity", identity,
		"leaseDuration", LeaderElectionLeaseDuration,
		"renewDeadline", LeaderElectionRenewDeadline,
		"retryPeriod", LeaderElectionRetryPeriod)

	elector.Run(ctx)

	select {
	case <-loopStarted:
		// Run cancels the leading context before returning, so the loop is
		// already unwinding; wait for it to stop writing before exiting.
		<-loopDone
	default:
	}

	if ctx.Err() != nil {
		return nil
	}

	// The election stopped while the daemon is still supposed to run, which
	// means the lease was lost (API server unreachable, conflicting writer,
	// etc.). Exit rather than keep a replica that has silently stopped
	// reconciling.
	return errors.New("leader election stopped unexpectedly: leadership was lost")
}
