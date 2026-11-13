// SPDX-License-Identifier: GPL-3.0-only
package pvcguardian

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/client-go/tools/record"

	"github.com/laghoule/pvcguardian/pkg/config"
	"github.com/laghoule/pvcguardian/pkg/k8s"
)

// ReconcileOutcome classifies the result of one reconciliation cycle.
type ReconcileOutcome string

const (
	// OutcomeSuccess means every namespace was scanned and every schedule
	// operation succeeded.
	OutcomeSuccess ReconcileOutcome = "success"
	// OutcomePartial means the cycle completed but at least one discovery,
	// build, or schedule write operation failed. Healthy schedules were still
	// reconciled and orphan deletion remained scoped to scanned namespaces.
	OutcomePartial ReconcileOutcome = "partial"
	// OutcomeFailed means the cycle could not establish a trustworthy view of
	// the cluster (for example listing namespaces or schedules failed).
	OutcomeFailed ReconcileOutcome = "error"
)

// Reconciler manages the lifecycle of Velero Schedules based on cluster state and config.
type Reconciler struct {
	cfg          *config.Config
	configPath   string
	discovery    *k8s.Discovery
	veleroClient k8s.VeleroClient
	recorder     record.EventRecorder
	metrics      *Metrics
	logger       *slog.Logger
	builder      *ScheduleBuilder

	// cfgMu protects cfg and builder, which are replaced by reloadConfig while
	// the health probe may read the reconcile interval concurrently.
	cfgMu sync.RWMutex

	mu              sync.RWMutex
	startTime       time.Time
	lastSuccessTime time.Time

	// leader is set while this replica holds the leader-election lease.
	leader atomic.Bool
}

// ReconcileResult summarizes the outcome of one reconciliation cycle.
type ReconcileResult struct {
	OptInNamespaces    int
	DiscoveredReleases int
	CreatedSchedules   int
	UpdatedSchedules   int
	DeletedSchedules   int
	SchedulesManaged   int
	SchedulesUnmanaged int
	SkippedOptOut      int
	SkippedNoPVC       int
	SkippedConflict    int
	UncoveredPVCs      int
	IndirectPVCs       int
	ConflictingUnits   int
	DiscoveryErrors    int
	BuildErrors        int
	CreateErrors       int
	UpdateErrors       int
	DeleteErrors       int
	NameCollisions     int
	ConfigReloadErrors int
	Outcome            ReconcileOutcome
}

// SkippedTotal returns the total number of releases skipped in the cycle.
func (r *ReconcileResult) SkippedTotal() int {
	return r.SkippedOptOut + r.SkippedNoPVC + r.SkippedConflict
}

// OperationErrors returns the number of configuration, schedule build, or
// schedule write failures.
func (r *ReconcileResult) OperationErrors() int {
	return r.ConfigReloadErrors + r.BuildErrors + r.CreateErrors + r.UpdateErrors + r.DeleteErrors + r.NameCollisions
}

// HasErrors reports whether the cycle had any non-fatal failure.
func (r *ReconcileResult) HasErrors() bool {
	return r.DiscoveryErrors > 0 || r.OperationErrors() > 0
}

// NewReconciler creates a Reconciler.
func NewReconciler(cfg *config.Config, configPath string, discovery *k8s.Discovery,
	veleroClient k8s.VeleroClient, recorder record.EventRecorder, metrics *Metrics, logger *slog.Logger) *Reconciler {

	if logger == nil {
		logger = slog.Default()
	}

	return &Reconciler{
		cfg:          cfg,
		configPath:   configPath,
		discovery:    discovery,
		veleroClient: veleroClient,
		recorder:     recorder,
		metrics:      metrics,
		logger:       logger,
		builder:      NewScheduleBuilder(cfg),
		startTime:    time.Now(),
	}
}

// reloadConfig attempts to reload the config from disk if a path is configured.
// The new configuration is validated by config.Load before being swapped in, so
// a malformed file leaves the previous configuration and builder in place. It
// reports whether the configuration in use is current.
func (r *Reconciler) reloadConfig() bool {
	if r.configPath == "" {
		return true
	}

	newCfg, err := config.Load(r.configPath)
	if err != nil {
		r.logger.Error("Failed to reload configuration from file, keeping existing config",
			"path", r.configPath,
			"error", err)
		return false
	}

	r.cfgMu.Lock()
	r.cfg = newCfg
	r.builder = NewScheduleBuilder(newCfg)
	r.cfgMu.Unlock()
	r.logger.Debug("Configuration reloaded successfully")
	return true
}

// currentConfig returns the configuration snapshot for the current operation.
func (r *Reconciler) currentConfig() *config.Config {
	r.cfgMu.RLock()
	defer r.cfgMu.RUnlock()
	return r.cfg
}

// currentBuilder returns the schedule builder matching currentConfig.
func (r *Reconciler) currentBuilder() *ScheduleBuilder {
	r.cfgMu.RLock()
	defer r.cfgMu.RUnlock()
	return r.builder
}

// ReconcileOnce performs a single pass over scanned namespaces, discovers releases,
// creates/updates desired schedules, and deletes orphaned ones.
func (r *Reconciler) ReconcileOnce(ctx context.Context) (result *ReconcileResult, err error) {
	configReloaded := r.reloadConfig()

	// Snapshot the configuration for the whole cycle. reloadConfig may swap it
	// concurrently, but a single cycle must use one coherent configuration.
	cfg := r.currentConfig()
	builder := r.currentBuilder()

	start := time.Now()
	result = &ReconcileResult{}
	if !configReloaded {
		result.ConfigReloadErrors = 1
	}
	defer func() {
		if r.metrics != nil {
			r.metrics.ObserveCycle(time.Since(start), result, err)
		}
	}()

	r.logger.Info("Starting reconciliation cycle", "optInLabel", k8s.OptInLabel)

	namespaces, err := r.discovery.ListScannableNamespaces(ctx, cfg.Discovery.ExcludeNamespaces)
	if err != nil {
		result.Outcome = OutcomeFailed
		return result, fmt.Errorf("listing scannable namespaces: %w", err)
	}
	result.OptInNamespaces = len(namespaces)
	r.logger.Info("Scannable namespaces identified", "count", len(namespaces), "namespaces", namespaces)

	desiredSchedules := make(map[string]*velerov1.Schedule)
	scheduleNames := make(map[string]string)
	scannedNamespaces := make(map[string]bool)
	var discoveryErrCount int
	totalUncovered := 0
	totalIndirect := 0
	totalConflicts := 0

	for _, ns := range namespaces {
		releases, uncovered, conflicts, err := r.discovery.DiscoverReleases(ctx, ns)
		if err != nil {
			discoveryErrCount++
			r.logger.Error("Failed to discover releases in namespace; skipping schedule cleanup for this namespace", "namespace", ns, "error", err)
			continue
		}
		scannedNamespaces[ns] = true
		totalConflicts += conflicts
		result.SkippedConflict += conflicts

		result.DiscoveredReleases += len(releases)

		if len(uncovered) > 0 {
			totalUncovered += len(uncovered)
			r.logger.Warn("Discovered PVCs not matched to any backup unit in namespace",
				"namespace", ns,
				"uncovered_count", len(uncovered),
				"pvcs", uncovered)
		}

		for _, rel := range releases {
			// Require explicit opt-in label on the release
			if !rel.OptIn {
				r.logger.Debug("Skipping release without opt-in label",
					"namespace", ns, "release", rel.Name)
				result.SkippedOptOut++
				continue
			}

			if cfg.Discovery.RequirePVC && len(rel.PVCs) == 0 {
				r.logger.Info("Skipping release with no PersistentVolumeClaims (requirePVC=true)",
					"namespace", ns, "release", rel.Name)
				result.SkippedNoPVC++
				continue
			}

			sch, err := builder.BuildSchedule(rel)
			if err != nil {
				result.BuildErrors++
				r.logger.Error("Failed to build schedule for release",
					"namespace", ns, "release", rel.Name, "error", err)
				continue
			}

			// Only report indirect coverage for units that actually get a
			// Schedule, so the metric reflects active backup scope.
			if len(rel.IndirectPVCs) > 0 {
				totalIndirect += len(rel.IndirectPVCs)
				r.logger.Warn("PVCs associated with backup unit through workload references but not directly selectable by its Schedule; restore coverage must be validated",
					"namespace", ns,
					"release", rel.Name,
					"indirect_count", len(rel.IndirectPVCs),
					"pvcs", rel.IndirectPVCs)
			}

			key := fmt.Sprintf("%s/%s", rel.Namespace, rel.Name)
			if previous, exists := scheduleNames[sch.Name]; exists && previous != key {
				result.NameCollisions++
				r.logger.Error("Schedule name collision; refusing to overwrite another backup unit",
					"schedule", sch.Name,
					"firstSource", previous,
					"secondSource", key)
				continue
			}
			scheduleNames[sch.Name] = key
			desiredSchedules[key] = sch
		}
	}
	result.UncoveredPVCs = totalUncovered
	result.IndirectPVCs = totalIndirect
	result.ConflictingUnits = totalConflicts
	result.DiscoveryErrors = discoveryErrCount

	// List every schedule in the Velero namespace so unmanaged ones (created
	// manually or by other tooling) can be reported, then keep only the
	// PVCGuardian-managed ones for reconciliation.
	allSchedules, err := r.veleroClient.ListSchedules(ctx, cfg.Velero.Namespace, "")
	if err != nil {
		result.Outcome = OutcomeFailed
		return result, fmt.Errorf("listing existing schedules: %w", err)
	}

	existingList := make([]velerov1.Schedule, 0, len(allSchedules))
	for _, sch := range allSchedules {
		if sch.Labels[ManagedByLabel] == ManagedByValue {
			existingList = append(existingList, sch)
		}
	}
	result.SchedulesUnmanaged = len(allSchedules) - len(existingList)

	existingBySource := make(map[string]*velerov1.Schedule)
	for i := range existingList {
		sch := &existingList[i]
		srcNs := sch.Annotations[SourceNamespaceAnno]
		srcRel := sch.Annotations[SourceReleaseAnno]
		if srcNs != "" && srcRel != "" {
			key := fmt.Sprintf("%s/%s", srcNs, srcRel)
			existingBySource[key] = sch
		}
	}

	for key, desired := range desiredSchedules {
		existing, found := existingBySource[key]
		if !found {
			created, err := r.veleroClient.CreateSchedule(ctx, desired)
			if err != nil {
				result.CreateErrors++
				r.logger.Error("Failed to create schedule",
					"name", desired.Name,
					"namespace", desired.Namespace,
					"error", err)
				continue
			}
			result.CreatedSchedules++
			r.logger.Info("Created Velero schedule",
				"schedule", created.Name,
				"cron", created.Spec.Schedule,
				"source", key,
				"slot", desired.Annotations[SlotAnno])
			r.recordEvent("Normal", "ScheduleCreated", fmt.Sprintf("Created schedule %s for release %s (cron: %s)", created.Name, key, created.Spec.Schedule))
			continue
		}

		if existing.Name != desired.Name {
			r.logger.Info("Schedule name changed, creating new before deleting old",
				"oldName", existing.Name, "newName", desired.Name)

			created, err := r.veleroClient.CreateSchedule(ctx, desired)
			if err != nil {
				// Never delete the old schedule when the replacement could not
				// be created. Retrying next cycle is safe.
				result.CreateErrors++
				r.logger.Error("Failed to create replacement schedule after rename; keeping old schedule",
					"oldName", existing.Name,
					"newName", desired.Name,
					"error", err)
				continue
			}
			result.CreatedSchedules++

			if err := r.veleroClient.DeleteSchedule(ctx, existing.Namespace, existing.Name); err != nil {
				result.DeleteErrors++
				r.logger.Error("Failed to delete old schedule after rename",
					"schedule", existing.Name,
					"error", err)
			} else {
				result.DeletedSchedules++
			}
			r.logger.Info("Replaced renamed schedule",
				"schedule", created.Name,
				"oldName", existing.Name,
				"source", key)
			continue
		}

		if r.needsUpdate(existing, desired) {
			desired.ResourceVersion = existing.ResourceVersion
			updated, err := r.veleroClient.UpdateSchedule(ctx, desired)
			if err != nil {
				result.UpdateErrors++
				r.logger.Error("Failed to update schedule",
					"name", desired.Name,
					"namespace", desired.Namespace,
					"error", err)
				continue
			}
			result.UpdatedSchedules++
			r.logger.Info("Updated Velero schedule",
				"schedule", updated.Name,
				"cron", updated.Spec.Schedule,
				"source", key)
			r.recordEvent("Normal", "ScheduleUpdated", fmt.Sprintf("Updated schedule %s for release %s", updated.Name, key))
		}
	}

	// 5. Delete orphaned schedules (existing in PVCGuardian management, but no longer desired).
	// Only delete schedules for namespaces that were successfully scanned to avoid
	// pruning schedules due to transient discovery errors.
	for key, existing := range existingBySource {
		srcNs := existing.Annotations[SourceNamespaceAnno]
		if !scannedNamespaces[srcNs] {
			remove, err := r.removedNamespace(ctx, srcNs, cfg.Discovery.ExcludeNamespaces)
			if err != nil {
				result.DiscoveryErrors++
				r.logger.Error("Failed to determine whether namespace still exists; preserving its schedule",
					"schedule", existing.Name,
					"namespace", srcNs,
					"error", err)
				continue
			}
			if !remove {
				r.logger.Warn("Preserving schedule for unscanned namespace due to discovery failure",
					"schedule", existing.Name,
					"namespace", srcNs,
					"source", key)
				continue
			}
			r.logger.Warn("Deleting orphaned schedule for a deleted or excluded namespace. Existing Backups are preserved.",
				"schedule", existing.Name,
				"namespace", srcNs,
				"source", key)
			if err := r.veleroClient.DeleteSchedule(ctx, existing.Namespace, existing.Name); err != nil {
				result.DeleteErrors++
				r.logger.Error("Failed to delete schedule for deleted or excluded namespace", "schedule", existing.Name, "error", err)
				continue
			}
			result.DeletedSchedules++
			r.recordEvent("Warning", "ScheduleDeleted", fmt.Sprintf("Deleted schedule %s for %s (namespace deleted or excluded)", existing.Name, key))
			continue
		}

		if _, desired := desiredSchedules[key]; !desired {
			r.logger.Warn("Deleting orphaned schedule (release removed or opt-out applied). Existing Backups are preserved.",
				"schedule", existing.Name,
				"source", key)

			if err := r.veleroClient.DeleteSchedule(ctx, existing.Namespace, existing.Name); err != nil {
				result.DeleteErrors++
				r.logger.Error("Failed to delete orphaned schedule", "schedule", existing.Name, "error", err)
				continue
			}
			result.DeletedSchedules++
			r.recordEvent("Warning", "ScheduleDeleted", fmt.Sprintf("Deleted schedule %s for %s", existing.Name, key))
		}
	}

	// The gauge is derived from the observed state rather than accumulated
	// deltas, so it is correct after a daemon restart.
	result.SchedulesManaged = len(existingList) + result.CreatedSchedules - result.DeletedSchedules

	if result.HasErrors() {
		result.Outcome = OutcomePartial
	} else {
		result.Outcome = OutcomeSuccess
	}

	r.logger.Info("Reconciliation cycle complete",
		"duration", time.Since(start),
		"outcome", result.Outcome,
		"optInNamespaces", result.OptInNamespaces,
		"discoveredReleases", result.DiscoveredReleases,
		"created", result.CreatedSchedules,
		"updated", result.UpdatedSchedules,
		"deleted", result.DeletedSchedules,
		"managedSchedules", result.SchedulesManaged,
		"unmanagedSchedules", result.SchedulesUnmanaged,
		"skipped", result.SkippedTotal(),
		"uncoveredPVCs", result.UncoveredPVCs,
		"indirectPVCs", result.IndirectPVCs,
		"configReloadErrors", result.ConfigReloadErrors,
		"discoveryErrors", result.DiscoveryErrors,
		"buildErrors", result.BuildErrors,
		"createErrors", result.CreateErrors,
		"updateErrors", result.UpdateErrors,
		"deleteErrors", result.DeleteErrors,
		"nameCollisions", result.NameCollisions)

	if result.HasErrors() {
		return result, fmt.Errorf(
			"reconciliation partially failed: %d config reload error(s), %d discovery error(s), %d build error(s), %d create error(s), %d update error(s), %d delete error(s), %d name collision(s)",
			result.ConfigReloadErrors, result.DiscoveryErrors, result.BuildErrors, result.CreateErrors, result.UpdateErrors, result.DeleteErrors, result.NameCollisions)
	}

	r.mu.Lock()
	r.lastSuccessTime = time.Now()
	r.mu.Unlock()

	return result, nil
}

// removedNamespace reports whether a source namespace is intentionally no
// longer managed (excluded) or no longer exists. An existing namespace that was
// not scanned is treated as a transient discovery failure and is preserved.
func (r *Reconciler) removedNamespace(ctx context.Context, namespace string, excluded []string) (bool, error) {
	for _, pattern := range excluded {
		if strings.TrimSpace(pattern) == namespace {
			return true, nil
		}
	}

	exists, err := r.discovery.NamespaceExists(ctx, namespace)
	if err != nil {
		return false, err
	}
	return !exists, nil
}

// needsUpdate checks whether any field that affects backup behavior differs
// between the existing and desired Schedule.
func (r *Reconciler) needsUpdate(existing, desired *velerov1.Schedule) bool {
	if existing.Spec.Schedule != desired.Spec.Schedule {
		return true
	}

	if existing.Spec.Template.TTL != desired.Spec.Template.TTL {
		return true
	}

	if existing.Spec.Template.StorageLocation != desired.Spec.Template.StorageLocation {
		return true
	}

	if existing.Annotations[PresetAnno] != desired.Annotations[PresetAnno] {
		return true
	}

	if !apiequality.Semantic.DeepEqual(existing.Spec.Template.IncludedNamespaces, desired.Spec.Template.IncludedNamespaces) {
		return true
	}

	// Selectors are the actual backup scope for selector-based units. A
	// changed annotation must be applied to the existing Schedule.
	if !apiequality.Semantic.DeepEqual(existing.Spec.Template.LabelSelector, desired.Spec.Template.LabelSelector) {
		return true
	}

	if !apiequality.Semantic.DeepEqual(existing.Spec.Template.OrLabelSelectors, desired.Spec.Template.OrLabelSelectors) {
		return true
	}

	if !apiequality.Semantic.DeepEqual(existing.Spec.Template.DefaultVolumesToFsBackup, desired.Spec.Template.DefaultVolumesToFsBackup) {
		return true
	}

	if !apiequality.Semantic.DeepEqual(existing.Spec.Template.SnapshotVolumes, desired.Spec.Template.SnapshotVolumes) {
		return true
	}

	return false
}

// healthWindow returns the maximum age of a successful reconciliation before
// the daemon is considered unready. It follows the reconcile interval (3x) with
// a floor of 5 minutes.
func (r *Reconciler) healthWindow() time.Duration {
	interval := r.currentConfig().Reconcile.Interval
	window := 3 * interval
	if window < 5*time.Minute {
		window = 5 * time.Minute
	}
	return window
}

// IsHealthy reports whether the reconciler has completed a successful
// reconciliation cycle recently. Before the first success, the startup time is
// used so a daemon that never reconciles does not stay healthy forever.
func (r *Reconciler) IsHealthy() bool {
	window := r.healthWindow()

	r.mu.RLock()
	reference := r.lastSuccessTime
	if reference.IsZero() {
		reference = r.startTime
	}
	r.mu.RUnlock()

	if reference.IsZero() {
		return false
	}
	return time.Since(reference) <= window
}

// recordEvent publishes an event if a recorder is configured.
func (r *Reconciler) recordEvent(eventType, reason, message string) {
	if r.recorder != nil {
		r.recorder.Event(&corev1.ObjectReference{
			Kind:      "PVCGuardian",
			Name:      "pvcguardian",
			Namespace: r.currentConfig().Velero.Namespace,
		}, eventType, reason, message)
	}
}

// RunLoop runs the reconciler periodically until the context is canceled.
func (r *Reconciler) RunLoop(ctx context.Context) error {
	interval := r.currentConfig().Reconcile.Interval
	r.logger.Info("Starting PVCGuardian reconciliation loop", "interval", interval)

	// First run immediately
	if _, err := r.ReconcileOnce(ctx); err != nil {
		r.logger.Error("Error during initial reconciliation", "error", err)
	}

	// ReconcileOnce reloads the configuration, so pick up an interval change
	// immediately instead of waiting for the next tick of the old ticker.
	interval = r.currentConfig().Reconcile.Interval
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			r.logger.Info("Reconciliation loop stopping")
			return ctx.Err()
		case <-ticker.C:
			if _, err := r.ReconcileOnce(ctx); err != nil {
				r.logger.Error("Error during scheduled reconciliation", "error", err)
			}

			if newInterval := r.currentConfig().Reconcile.Interval; newInterval != interval {
				interval = newInterval
				ticker.Reset(interval)
				r.logger.Info("Reconcile interval updated", "interval", interval)
			}
		}
	}
}
