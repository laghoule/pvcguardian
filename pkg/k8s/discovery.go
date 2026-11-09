// SPDX-License-Identifier: GPL-3.0-only
package k8s

import (
	"context"
	"fmt"
	"log/slog"
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
)

// DiscoveredRelease represents an identified Helm release.
type DiscoveredRelease struct {
	Namespace       string
	Name            string
	Chart           string
	PVCs            []string // PVC names belonging to this release
	OptIn           bool     // true if explicitly opted in via label="true"
	AnnotatedPreset string   // preset specified in pvcguardian.io/preset annotation
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
func (d *Discovery) DiscoverReleases(ctx context.Context, namespace, optInLabel, presetAnnotation, releaseLabel string) ([]DiscoveredRelease, []string, error) {
	type releaseMeta struct {
		chart    string
		optIn    bool
		preset   string
		claims   map[string]struct{}
		prefixes []string
	}
	releases := make(map[string]*releaseMeta)
	workloadOwner := make(map[types.UID]string)

	recordWorkload := func(kind, name string, meta metav1.ObjectMeta, claimNames, claimPrefixes []string) {
		if meta.Labels == nil {
			return
		}
		if !strings.EqualFold(meta.Labels[HelmManagedByLabel], HelmManagedByValue) {
			return
		}

		instance := meta.Labels[HelmInstanceLabel]
		if instance == "" {
			d.logger.Warn("Helm-managed workload is missing instance label",
				"namespace", namespace,
				"kind", kind,
				"name", name)
			return
		}

		rm, exists := releases[instance]
		if !exists {
			rm = &releaseMeta{claims: make(map[string]struct{})}
			releases[instance] = rm
		}

		if meta.UID != "" {
			workloadOwner[meta.UID] = instance
		}

		if rm.chart == "" && meta.Labels[HelmChartLabel] != "" {
			rm.chart = meta.Labels[HelmChartLabel]
		}

		if val, ok := meta.Labels[optInLabel]; ok {
			if strings.EqualFold(strings.TrimSpace(val), "true") {
				rm.optIn = true
			}
		}

		if meta.Annotations != nil && rm.preset == "" {
			if p, ok := meta.Annotations[presetAnnotation]; ok {
				rm.preset = strings.TrimSpace(p)
			}
		}

		for _, claim := range claimNames {
			rm.claims[claim] = struct{}{}
		}
		rm.prefixes = append(rm.prefixes, claimPrefixes...)
	}

	deps, err := d.kubeClient.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("listing deployments in %s: %w", namespace, err)
	}
	for _, item := range deps.Items {
		recordWorkload("Deployment", item.Name, item.ObjectMeta, claimNamesFromVolumes(item.Spec.Template.Spec.Volumes), nil)
	}

	sts, err := d.kubeClient.AppsV1().StatefulSets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("listing statefulsets in %s: %w", namespace, err)
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
		return nil, nil, fmt.Errorf("listing daemonsets in %s: %w", namespace, err)
	}
	for _, item := range ds.Items {
		recordWorkload("DaemonSet", item.Name, item.ObjectMeta, claimNamesFromVolumes(item.Spec.Template.Spec.Volumes), nil)
	}

	cjs, err := d.kubeClient.BatchV1().CronJobs(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("listing cronjobs in %s: %w", namespace, err)
	}
	for _, item := range cjs.Items {
		recordWorkload("CronJob", item.Name, item.ObjectMeta, claimNamesFromVolumes(item.Spec.JobTemplate.Spec.Template.Spec.Volumes), nil)
	}

	jobs, err := d.kubeClient.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("listing jobs in %s: %w", namespace, err)
	}
	for _, item := range jobs.Items {
		recordWorkload("Job", item.Name, item.ObjectMeta, claimNamesFromVolumes(item.Spec.Template.Spec.Volumes), nil)
	}

	pvcList, err := d.kubeClient.CoreV1().PersistentVolumeClaims(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("listing PVCs in %s: %w", namespace, err)
	}

	pvcByRelease := make(map[string][]string)
	var uncoveredPVCs []string

	for _, pvc := range pvcList.Items {
		matched := make(map[string]struct{})

		// Explicit per-PVC release association takes precedence.
		if release := strings.TrimSpace(pvc.Labels[releaseLabel]); release != "" {
			if _, ok := releases[release]; ok {
				matched[release] = struct{}{}
			} else {
				d.logger.Warn("PVC references an unknown release",
					"namespace", namespace,
					"pvc", pvc.Name,
					"release", release,
					"label", releaseLabel)
			}
		}

		if len(matched) == 0 {
			if instance := pvc.Labels[HelmInstanceLabel]; instance != "" {
				if _, ok := releases[instance]; ok {
					matched[instance] = struct{}{}
				}
			}
		}

		if len(matched) == 0 {
			for name, rm := range releases {
				if _, ok := rm.claims[pvc.Name]; ok {
					matched[name] = struct{}{}
				}
			}
		}

		if len(matched) == 0 {
			for name, rm := range releases {
				for _, prefix := range rm.prefixes {
					if matchesStatefulSetClaim(pvc.Name, prefix) {
						matched[name] = struct{}{}
						break
					}
				}
			}
		}

		if len(matched) == 0 {
			for _, ref := range pvc.OwnerReferences {
				if name, ok := workloadOwner[ref.UID]; ok {
					matched[name] = struct{}{}
				}
			}
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
			OptIn:           meta.optIn,
			AnnotatedPreset: meta.preset,
		})
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})
	sort.Strings(uncoveredPVCs)

	return result, uncoveredPVCs, nil
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
