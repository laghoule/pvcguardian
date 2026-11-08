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
	Namespace string
	Name      string
	Chart     string
	PVCs      []string // PVC names belonging to this release
	OptOut    bool     // true if explicitly opted out via label="false"
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

// FindOptInNamespaces lists namespaces with optInLabel == "true".
func (d *Discovery) FindOptInNamespaces(ctx context.Context, optInLabel string) ([]string, error) {
	nsList, err := d.kubeClient.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing namespaces: %w", err)
	}

	var optedIn []string
	for _, ns := range nsList.Items {
		val, exists := ns.Labels[optInLabel]
		if exists && strings.EqualFold(strings.TrimSpace(val), "true") {
			optedIn = append(optedIn, ns.Name)
		}
	}
	sort.Strings(optedIn)
	return optedIn, nil
}

// DiscoverReleases scans workloads in a namespace and associates their PVCs.
func (d *Discovery) DiscoverReleases(ctx context.Context, namespace string, optInLabel string) ([]DiscoveredRelease, []string, error) {
	// 1. Identify releases from Deployments, StatefulSets, DaemonSets, CronJobs, Jobs
	type releaseMeta struct {
		chart  string
		optOut bool
	}
	releases := make(map[string]*releaseMeta)
	unlabeledCount := 0

	recordWorkload := func(kind, name string, labels map[string]string) {
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
			if strings.EqualFold(strings.TrimSpace(val), "false") {
				rm.optOut = true
			}
		}
	}

	deps, err := d.kubeClient.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("listing deployments in %s: %w", namespace, err)
	}
	for _, item := range deps.Items {
		recordWorkload("Deployment", item.Name, item.Labels)
	}

	sts, err := d.kubeClient.AppsV1().StatefulSets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("listing statefulsets in %s: %w", namespace, err)
	}
	for _, item := range sts.Items {
		recordWorkload("StatefulSet", item.Name, item.Labels)
	}

	ds, err := d.kubeClient.AppsV1().DaemonSets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("listing daemonsets in %s: %w", namespace, err)
	}
	for _, item := range ds.Items {
		recordWorkload("DaemonSet", item.Name, item.Labels)
	}

	cjs, err := d.kubeClient.BatchV1().CronJobs(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("listing cronjobs in %s: %w", namespace, err)
	}
	for _, item := range cjs.Items {
		recordWorkload("CronJob", item.Name, item.Labels)
	}

	jobs, err := d.kubeClient.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("listing jobs in %s: %w", namespace, err)
	}
	for _, item := range jobs.Items {
		recordWorkload("Job", item.Name, item.Labels)
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
			Namespace: namespace,
			Name:      name,
			Chart:     meta.chart,
			PVCs:      pvcs,
			OptOut:    meta.optOut,
		})
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})
	sort.Strings(uncoveredPVCs)

	return result, uncoveredPVCs, nil
}
