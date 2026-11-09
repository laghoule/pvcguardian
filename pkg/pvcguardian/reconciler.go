// SPDX-License-Identifier: GPL-3.0-only
package pvcguardian

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"time"

	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"

	"github.com/laghoule/pvcguardian/pkg/config"
	"github.com/laghoule/pvcguardian/pkg/k8s"
)

// Reconciler manages the lifecycle of Velero Schedules based on cluster state and config.
type Reconciler struct {
	cfg          *config.Config
	configPath   string
	discovery    *k8s.Discovery
	veleroClient k8s.VeleroClient
	recorder     record.EventRecorder
	logger       *slog.Logger
	builder      *ScheduleBuilder
}

// ReconcileResult summarizes the outcome of one reconciliation cycle.
type ReconcileResult struct {
	OptInNamespaces    int
	DiscoveredReleases int
	CreatedSchedules   int
	UpdatedSchedules   int
	DeletedSchedules   int
	SkippedReleases    int
	UncoveredPVCs      int
}

// NewReconciler creates a Reconciler.
func NewReconciler(
	cfg *config.Config,
	configPath string,
	discovery *k8s.Discovery,
	veleroClient k8s.VeleroClient,
	recorder record.EventRecorder,
	logger *slog.Logger,
) *Reconciler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Reconciler{
		cfg:          cfg,
		configPath:   configPath,
		discovery:    discovery,
		veleroClient: veleroClient,
		recorder:     recorder,
		logger:       logger,
		builder:      NewScheduleBuilder(cfg),
	}
}

// reloadConfig attempts to reload the config from disk if a path is configured.
func (r *Reconciler) reloadConfig() {
	if r.configPath == "" {
		return
	}
	newCfg, err := config.Load(r.configPath)
	if err != nil {
		r.logger.Error("Failed to reload configuration from file, keeping existing config",
			"path", r.configPath,
			"error", err)
		return
	}
	r.cfg = newCfg
	r.builder = NewScheduleBuilder(newCfg)
	r.logger.Debug("Configuration reloaded successfully")
}

// ReconcileOnce performs a single pass over opt-in namespaces, discovers releases,
// creates/updates desired schedules, and deletes orphaned ones.
func (r *Reconciler) ReconcileOnce(ctx context.Context) (*ReconcileResult, error) {
	r.reloadConfig()

	result := &ReconcileResult{}
	start := time.Now()

	r.logger.Info("Starting reconciliation cycle", "optInLabel", r.cfg.Selectors.Label)

	namespaces, err := r.discovery.ListScannableNamespaces(ctx, r.cfg.Selectors.ExcludeNamespaces)
	if err != nil {
		return nil, fmt.Errorf("listing scannable namespaces: %w", err)
	}
	result.OptInNamespaces = len(namespaces)
	r.logger.Info("Scannable namespaces identified", "count", len(namespaces), "namespaces", namespaces)

	desiredSchedules := make(map[string]*velerov1.Schedule)
	totalUncovered := 0

	for _, ns := range namespaces {
		releases, uncovered, err := r.discovery.DiscoverReleases(ctx, ns, r.cfg.Selectors.Label, r.cfg.Selectors.PresetAnnotation)
		if err != nil {
			r.logger.Error("Failed to discover releases in namespace", "namespace", ns, "error", err)
			continue
		}

		result.DiscoveredReleases += len(releases)

		if len(uncovered) > 0 {
			totalUncovered += len(uncovered)
			r.logger.Warn("Discovered PVCs not matched to any Helm release in namespace",
				"namespace", ns,
				"uncovered_count", len(uncovered),
				"pvcs", uncovered)
		}

		for _, rel := range releases {
			// Require explicit opt-in label on the release
			if !rel.OptIn {
				r.logger.Debug("Skipping release without opt-in label",
					"namespace", ns, "release", rel.Name)
				result.SkippedReleases++
				continue
			}

			if r.cfg.Selectors.RequirePVC && len(rel.PVCs) == 0 {
				r.logger.Info("Skipping release with no PersistentVolumeClaims (requirePVC=true)",
					"namespace", ns, "release", rel.Name)
				result.SkippedReleases++
				continue
			}

			sch, err := r.builder.BuildSchedule(rel)
			if err != nil {
				r.logger.Error("Failed to build schedule for release",
					"namespace", ns, "release", rel.Name, "error", err)
				continue
			}

			key := fmt.Sprintf("%s/%s", rel.Namespace, rel.Name)
			desiredSchedules[key] = sch
		}
	}
	result.UncoveredPVCs = totalUncovered

	labelSel := fmt.Sprintf("%s=%s", ManagedByLabel, ManagedByValue)
	existingList, err := r.veleroClient.ListSchedules(ctx, r.cfg.Velero.Namespace, labelSel)
	if err != nil {
		return nil, fmt.Errorf("listing existing schedules: %w", err)
	}

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
		} else {
			if existing.Name != desired.Name {
				r.logger.Info("Schedule name changed, creating new before deleting old",
					"oldName", existing.Name, "newName", desired.Name)
				if _, err := r.veleroClient.CreateSchedule(ctx, desired); err == nil {
					_ = r.veleroClient.DeleteSchedule(ctx, existing.Namespace, existing.Name)
					result.CreatedSchedules++
					result.DeletedSchedules++
					continue
				}
			}

			if r.needsUpdate(existing, desired) {
				desired.ResourceVersion = existing.ResourceVersion
				updated, err := r.veleroClient.UpdateSchedule(ctx, desired)
				if err != nil {
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
	}

	// 5. Delete orphaned schedules (existing in PVCGuardian management, but no longer desired)
	for key, existing := range existingBySource {
		if _, desired := desiredSchedules[key]; !desired {
			r.logger.Warn("Deleting orphaned schedule (release removed or opt-out applied). Existing Backups are preserved.",
				"schedule", existing.Name,
				"source", key)

			if err := r.veleroClient.DeleteSchedule(ctx, existing.Namespace, existing.Name); err != nil {
				r.logger.Error("Failed to delete orphaned schedule", "schedule", existing.Name, "error", err)
				continue
			}
			result.DeletedSchedules++
			r.recordEvent("Warning", "ScheduleDeleted", fmt.Sprintf("Deleted schedule %s for %s", existing.Name, key))
		}
	}

	r.logger.Info("Reconciliation cycle complete",
		"duration", time.Since(start),
		"optInNamespaces", result.OptInNamespaces,
		"discoveredReleases", result.DiscoveredReleases,
		"created", result.CreatedSchedules,
		"updated", result.UpdatedSchedules,
		"deleted", result.DeletedSchedules,
		"skipped", result.SkippedReleases,
		"uncoveredPVCs", result.UncoveredPVCs)

	return result, nil
}

// needsUpdate checks whether meaningful fields (schedule, ttl, annotations) differ.
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
	if !reflect.DeepEqual(existing.Spec.Template.IncludedNamespaces, desired.Spec.Template.IncludedNamespaces) {
		return true
	}
	return false
}

// recordEvent publishes an event if a recorder is configured.
func (r *Reconciler) recordEvent(eventType, reason, message string) {
	if r.recorder != nil {
		r.recorder.Eventf(&corev1.ObjectReference{
			Kind:      "PVCGuardian",
			Name:      "pvcguardian",
			Namespace: r.cfg.Velero.Namespace,
		}, eventType, reason, message)
	}
}

// RunLoop runs the reconciler periodically until the context is canceled.
func (r *Reconciler) RunLoop(ctx context.Context) error {
	r.logger.Info("Starting PVCGuardian reconciliation loop", "interval", r.cfg.Reconcile.Interval)

	// First run immediately
	if _, err := r.ReconcileOnce(ctx); err != nil {
		r.logger.Error("Error during initial reconciliation", "error", err)
	}

	ticker := time.NewTicker(r.cfg.Reconcile.Interval)
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
		}
	}
}
