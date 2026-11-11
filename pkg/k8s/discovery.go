// SPDX-License-Identifier: GPL-3.0-only
package k8s

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

const (
	HelmManagedByLabel = "app.kubernetes.io/managed-by"
	HelmInstanceLabel  = "app.kubernetes.io/instance"
	HelmChartLabel     = "helm.sh/chart"
	HelmManagedByValue = "Helm"

	// OptInLabel marks a workload as explicitly opted in to PVCGuardian.
	OptInLabel = "pvcguardian.io/enabled"
	// PresetAnnotation selects the schedule preset on a workload.
	PresetAnnotation = "pvcguardian.io/preset"
	// ReleaseLabel explicitly attaches a PVC to a Helm release by name.
	ReleaseLabel = "pvcguardian.io/release"
	// SelectorAnnotation declares a label selector (key=value[,key=value])
	// that defines a backup unit. It works on Helm-managed and non-Helm
	// workloads alike, and implies opt-in.
	SelectorAnnotation = "pvcguardian.io/selector"
)

// DiscoveredRelease represents an identified Helm release.
type DiscoveredRelease struct {
	Namespace       string
	Name            string
	Chart           string
	PVCs            []string          // PVC names belonging to this release
	OptIn           bool              // true if explicitly opted in via label="true"
	AnnotatedPreset string            // preset specified in pvcguardian.io/preset annotation
	Selector        map[string]string // matchLabels from pvcguardian.io/selector, nil for Helm-derived units
	Conflict        bool              // true if unit had conflicting selectors, presets, or opt-in/opt-out
}

// DiscoveryReport contains discovered releases, uncovered PVCs, and count of conflicting units.
type DiscoveryReport struct {
	Releases         []DiscoveredRelease
	UncoveredPVCs    []string
	ConflictingUnits int
}

// Discovery holds context and clients needed to scan workloads and PVCs.
type Discovery struct {
	kubeClient kubernetes.Interface
	logger     *slog.Logger
}

// NewDiscovery creates a Discovery instance.
func NewDiscovery(client kubernetes.Interface, logger *slog.Logger) *Discovery {
	if logger == nil {
		logger = slog.Default()
	}
	return &Discovery{
		kubeClient: client,
		logger:     logger,
	}
}

// ListScannableNamespaces returns all cluster namespaces except those in excluded list.
func (d *Discovery) ListScannableNamespaces(ctx context.Context, excluded []string) ([]string, error) {
	nsList, err := d.kubeClient.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing namespaces: %w", err)
	}

	excludeMap := make(map[string]bool)
	for _, ex := range excluded {
		excludeMap[strings.TrimSpace(ex)] = true
	}

	var scannable []string
	for _, ns := range nsList.Items {
		if !excludeMap[ns.Name] {
			scannable = append(scannable, ns.Name)
		}
	}
	sort.Strings(scannable)
	return scannable, nil
}

// DiscoverReleases scans workloads in a namespace and associates their PVCs.
// It returns discovered releases, uncovered PVC names, and the count of units skipped due to conflicts.
func (d *Discovery) DiscoverReleases(ctx context.Context, namespace string) ([]DiscoveredRelease, []string, int, error) {
	type releaseMeta struct {
		chart            string
		optInSet         bool
		optInVal         bool
		optInConflict    bool
		preset           string
		presetConflict   bool
		claims           map[string]struct{}
		prefixes         []string
		selector         map[string]string
		selectorRaw      string
		selectorConflict bool
	}
	releases := make(map[string]*releaseMeta)
	workloadOwner := make(map[types.UID]string)

	recordWorkload := func(kind, name string, meta metav1.ObjectMeta, claimNames, claimPrefixes []string) {
		rawSelector := ""
		if meta.Annotations != nil {
			rawSelector = strings.TrimSpace(meta.Annotations[SelectorAnnotation])
		}

		// A selector annotation is enough to declare a backup unit, even
		// when the workload is not Helm-managed.
		if !strings.EqualFold(meta.Labels[HelmManagedByLabel], HelmManagedByValue) && rawSelector == "" {
			return
		}

		instance := meta.Labels[HelmInstanceLabel]
		if instance == "" {
			if rawSelector == "" {
				d.logger.Warn("Helm-managed workload is missing instance label",
					"namespace", namespace,
					"kind", kind,
					"name", name)
				return
			}
			instance = name
		}

		var selector map[string]string
		if rawSelector != "" {
			var err error
			selector, err = parseSelectorAnnotation(rawSelector)
			if err != nil {
				d.logger.Warn("Ignoring workload with invalid selector annotation",
					"namespace", namespace,
					"kind", kind,
					"name", name,
					"annotation", SelectorAnnotation,
					"error", err)
				return
			}
		}

		rm, exists := releases[instance]
		if !exists {
			rm = &releaseMeta{claims: make(map[string]struct{})}
			releases[instance] = rm
		}

		if selector != nil {
			switch {
			case rm.selector == nil:
				rm.selector = selector
				rm.selectorRaw = rawSelector
			case !maps.Equal(rm.selector, selector):
				rm.selectorConflict = true
				d.logger.Warn("Conflicting selectors for the same backup unit",
					"namespace", namespace,
					"unit", instance,
					"existing", rm.selectorRaw,
					"conflicting", rawSelector)
			}
			// Declaring a selector implies opt-in unless explicitly opted out.
			if !rm.optInSet {
				rm.optInSet = true
				rm.optInVal = true
			}
		}

		if meta.UID != "" {
			workloadOwner[meta.UID] = instance
		}

		if rm.chart == "" && meta.Labels[HelmChartLabel] != "" {
			rm.chart = meta.Labels[HelmChartLabel]
		}

		if val, ok := meta.Labels[OptInLabel]; ok {
			trimmed := strings.ToLower(strings.TrimSpace(val))
			workloadOptIn := (trimmed == "true")
			if !rm.optInSet {
				rm.optInSet = true
				rm.optInVal = workloadOptIn
			} else if rm.optInVal != workloadOptIn {
				rm.optInConflict = true
				d.logger.Warn("Conflicting opt-in settings for the same backup unit",
					"namespace", namespace,
					"unit", instance,
					"existing", rm.optInVal,
					"conflicting", workloadOptIn)
			}
		}

		if meta.Annotations != nil {
			if p, ok := meta.Annotations[PresetAnnotation]; ok {
				trimmedPreset := strings.TrimSpace(p)
				if trimmedPreset != "" {
					if rm.preset == "" {
						rm.preset = trimmedPreset
					} else if rm.preset != trimmedPreset {
						rm.presetConflict = true
						d.logger.Warn("Conflicting presets for the same backup unit",
							"namespace", namespace,
							"unit", instance,
							"existing", rm.preset,
							"conflicting", trimmedPreset)
					}
				}
			}
		}

		for _, claim := range claimNames {
			rm.claims[claim] = struct{}{}
		}
		rm.prefixes = append(rm.prefixes, claimPrefixes...)
	}

	deps, err := d.kubeClient.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, 0, fmt.Errorf("listing deployments in %s: %w", namespace, err)
	}
	for _, item := range deps.Items {
		recordWorkload("Deployment", item.Name, item.ObjectMeta, claimNamesFromVolumes(item.Spec.Template.Spec.Volumes), nil)
	}

	sts, err := d.kubeClient.AppsV1().StatefulSets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, 0, fmt.Errorf("listing statefulsets in %s: %w", namespace, err)
	}
	for _, item := range sts.Items {
		var prefixes []string
		for _, vct := range item.Spec.VolumeClaimTemplates {
			if vct.Name != "" {
				prefixes = append(prefixes, vct.Name+"-"+item.Name+"-")
			}
		}
		recordWorkload("StatefulSet", item.Name, item.ObjectMeta, claimNamesFromVolumes(item.Spec.Template.Spec.Volumes), prefixes)
	}

	ds, err := d.kubeClient.AppsV1().DaemonSets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, 0, fmt.Errorf("listing daemonsets in %s: %w", namespace, err)
	}
	for _, item := range ds.Items {
		recordWorkload("DaemonSet", item.Name, item.ObjectMeta, claimNamesFromVolumes(item.Spec.Template.Spec.Volumes), nil)
	}

	cjs, err := d.kubeClient.BatchV1().CronJobs(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, 0, fmt.Errorf("listing cronjobs in %s: %w", namespace, err)
	}
	for _, item := range cjs.Items {
		recordWorkload("CronJob", item.Name, item.ObjectMeta, claimNamesFromVolumes(item.Spec.JobTemplate.Spec.Template.Spec.Volumes), nil)
	}

	jobs, err := d.kubeClient.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, 0, fmt.Errorf("listing jobs in %s: %w", namespace, err)
	}
	for _, item := range jobs.Items {
		recordWorkload("Job", item.Name, item.ObjectMeta, claimNamesFromVolumes(item.Spec.Template.Spec.Volumes), nil)
	}

	// Drop units whose workloads declared incompatible selectors, presets or opt-in.
	// Their PVCs will fall through to the uncovered list instead of silently belonging to no unit.
	conflictingUnits := 0
	for name, rm := range releases {
		if rm.selectorConflict || rm.presetConflict || rm.optInConflict {
			conflictingUnits++
			reason := "conflicting selectors"
			if rm.presetConflict {
				reason = "conflicting presets"
			} else if rm.optInConflict {
				reason = "conflicting opt-in settings"
			}
			d.logger.Warn("Skipping backup unit due to conflict",
				"namespace", namespace,
				"unit", name,
				"reason", reason)
			delete(releases, name)
		}
	}

	pvcList, err := d.kubeClient.CoreV1().PersistentVolumeClaims(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, 0, fmt.Errorf("listing PVCs in %s: %w", namespace, err)
	}

	pvcByRelease := make(map[string][]string)
	var uncoveredPVCs []string

	for _, pvc := range pvcList.Items {
		// Explicit per-PVC release association takes precedence.
		if release := strings.TrimSpace(pvc.Labels[ReleaseLabel]); release != "" {
			if _, ok := releases[release]; ok {
				pvcByRelease[release] = append(pvcByRelease[release], pvc.Name)
				continue
			}
			d.logger.Warn("PVC references an unknown release",
				"namespace", namespace,
				"pvc", pvc.Name,
				"release", release,
				"label", ReleaseLabel)
		}

		matched := make(map[string]struct{})

		// Selector-based units match PVCs directly by their labels. This is
		// the same criterion the Velero Schedule uses, so discovery and
		// backup stay consistent.
		for name, rm := range releases {
			if rm.selector != nil && matchesMatchLabels(rm.selector, pvc.Labels) {
				matched[name] = struct{}{}
			}
		}

		// Legacy Helm association only applies to units without an explicit
		// selector.
		legacyMatched := make(map[string]struct{})
		if instance := pvc.Labels[HelmInstanceLabel]; instance != "" {
			if rm, ok := releases[instance]; ok && rm.selector == nil {
				legacyMatched[instance] = struct{}{}
			}
		}

		if len(legacyMatched) == 0 {
			for name, rm := range releases {
				if rm.selector != nil {
					continue
				}
				if _, ok := rm.claims[pvc.Name]; ok {
					legacyMatched[name] = struct{}{}
				}
			}
		}

		if len(legacyMatched) == 0 {
			for name, rm := range releases {
				if rm.selector != nil {
					continue
				}
				for _, prefix := range rm.prefixes {
					if matchesStatefulSetClaim(pvc.Name, prefix) {
						legacyMatched[name] = struct{}{}
						break
					}
				}
			}
		}

		if len(legacyMatched) == 0 {
			for _, ref := range pvc.OwnerReferences {
				if name, ok := workloadOwner[ref.UID]; ok {
					if rm, ok := releases[name]; ok && rm.selector == nil {
						legacyMatched[name] = struct{}{}
					}
				}
			}
		}

		for name := range legacyMatched {
			matched[name] = struct{}{}
		}

		if len(matched) > 1 {
			names := make([]string, 0, len(matched))
			for name := range matched {
				names = append(names, name)
			}
			sort.Strings(names)
			d.logger.Warn("PVC is associated with multiple backup units",
				"namespace", namespace,
				"pvc", pvc.Name,
				"units", names)
		}

		if len(matched) == 0 {
			uncoveredPVCs = append(uncoveredPVCs, pvc.Name)
			continue
		}

		for name := range matched {
			pvcByRelease[name] = append(pvcByRelease[name], pvc.Name)
		}
	}

	var result []DiscoveredRelease
	for name, meta := range releases {
		pvcs := pvcByRelease[name]
		sort.Strings(pvcs)
		result = append(result, DiscoveredRelease{
			Namespace:       namespace,
			Name:            name,
			Chart:           meta.chart,
			PVCs:            pvcs,
			OptIn:           meta.optInVal,
			AnnotatedPreset: meta.preset,
			Selector:        maps.Clone(meta.selector),
		})
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})
	sort.Strings(uncoveredPVCs)

	return result, uncoveredPVCs, conflictingUnits, nil
}

// claimNamesFromVolumes extracts PVC claim names referenced by pod volumes.
func claimNamesFromVolumes(volumes []corev1.Volume) []string {
	var names []string
	for _, volume := range volumes {
		if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName != "" {
			names = append(names, volume.PersistentVolumeClaim.ClaimName)
		}
	}
	return names
}

// matchesStatefulSetClaim reports whether pvcName follows the
// "<volumeClaimTemplate>-<statefulSet>-<ordinal>" naming convention.
func matchesStatefulSetClaim(pvcName, prefix string) bool {
	rest, ok := strings.CutPrefix(pvcName, prefix)
	if !ok || rest == "" {
		return false
	}
	for _, r := range rest {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
