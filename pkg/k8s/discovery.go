// SPDX-License-Identifier: GPL-3.0-only
package k8s

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	Namespace        string
	Name             string
	Chart            string
	PVCs             []string // PVC names belonging to this release
	OptIn            bool     // true if explicitly opted in via label="true"
	AnnotatedPreset  string   // preset specified in pvcguardian.io/preset annotation
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
func (d *Discovery) DiscoverReleases(ctx context.Context, namespace, optInLabel, presetAnnotation string) ([]DiscoveredRelease, []string, error) {
	type releaseMeta struct {
		chart  string
		optIn  bool
		preset string
	}
	releases := make(map[string]*releaseMeta)
	unlabeledCount := 0

	recordWorkload := func(kind, name string, labels, annotations map[string]string) {
		if labels == nil {
			return
		}
		if !strings.EqualFold(labels[HelmManagedByLabel], HelmManagedByValue) {
			return
		}

		instance := labels[HelmInstanceLabel]
		if instance == "" {
			d.logger.Warn("Helm-managed workload is missing instance label",
				"namespace", namespace,
				"kind", kind,
				"name", name)
			unlabeledCount++
			return
		}

		rm, exists := releases[instance]
		if !exists {
			rm = &releaseMeta{}
			releases[instance] = rm
		}

		if rm.chart == "" && labels[HelmChartLabel] != "" {
			rm.chart = labels[HelmChartLabel]
		}

		if val, ok := labels[optInLabel]; ok {
			if strings.EqualFold(strings.TrimSpace(val), "true") {
				rm.optIn = true
			}
		}

		if annotations != nil && rm.preset == "" {
			if p, ok := annotations[presetAnnotation]; ok {
				rm.preset = strings.TrimSpace(p)
			}
		}
	}

	deps, err := d.kubeClient.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("listing deployments in %s: %w", namespace, err)
	}
	for _, item := range deps.Items {
		recordWorkload("Deployment", item.Name, item.Labels, item.Annotations)
	}

	sts, err := d.kubeClient.AppsV1().StatefulSets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("listing statefulsets in %s: %w", namespace, err)
	}
	for _, item := range sts.Items {
		recordWorkload("StatefulSet", item.Name, item.Labels, item.Annotations)
	}

	ds, err := d.kubeClient.AppsV1().DaemonSets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("listing daemonsets in %s: %w", namespace, err)
	}
	for _, item := range ds.Items {
		recordWorkload("DaemonSet", item.Name, item.Labels, item.Annotations)
	}

	cjs, err := d.kubeClient.BatchV1().CronJobs(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("listing cronjobs in %s: %w", namespace, err)
	}
	for _, item := range cjs.Items {
		recordWorkload("CronJob", item.Name, item.Labels, item.Annotations)
	}

	jobs, err := d.kubeClient.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("listing jobs in %s: %w", namespace, err)
	}
	for _, item := range jobs.Items {
		recordWorkload("Job", item.Name, item.Labels, item.Annotations)
	}

	pvcList, err := d.kubeClient.CoreV1().PersistentVolumeClaims(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("listing PVCs in %s: %w", namespace, err)
	}

	pvcByRelease := make(map[string][]string)
	var uncoveredPVCs []string

	for _, pvc := range pvcList.Items {
		instance := pvc.Labels[HelmInstanceLabel]
		if instance != "" && releases[instance] != nil {
			pvcByRelease[instance] = append(pvcByRelease[instance], pvc.Name)
		} else {
			uncoveredPVCs = append(uncoveredPVCs, pvc.Name)
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
