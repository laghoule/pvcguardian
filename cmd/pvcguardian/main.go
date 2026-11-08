// SPDX-License-Identifier: GPL-3.0-only
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/laghoule/pvcguardian/pkg/config"
	"github.com/laghoule/pvcguardian/pkg/k8s"
	"github.com/laghoule/pvcguardian/pkg/pvcguardian"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	var (
		configPath  = flag.String("config", "/etc/pvcguardian/config.yaml", "Path to PVCGuardian YAML configuration file")
		kubeconfig  = flag.String("kubeconfig", "", "Path to kubeconfig file (optional, uses in-cluster config if empty)")
		showVersion = flag.Bool("version", false, "Print PVCGuardian version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("PVCGuardian %s (commit: %s, built: %s)\n", version, commit, date)
		os.Exit(0)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load configuration: %v\n", err)
		os.Exit(1)
	}

	logger := pvcguardian.SetupLogger(cfg.Logging.Level, cfg.Logging.Format)
	logger.Info("Starting PVCGuardian daemon",
		"version", version,
		"commit", commit,
		"reconcileInterval", cfg.Reconcile.Interval,
		"optInLabel", cfg.Selectors.Label,
		"veleroNamespace", cfg.Velero.Namespace)

	restConfig, err := buildKubeConfig(*kubeconfig)
	if err != nil {
		logger.Error("Failed to build Kubernetes configuration", "error", err)
		os.Exit(1)
	}

	kubeClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		logger.Error("Failed to create Kubernetes clientset", "error", err)
		os.Exit(1)
	}

	dynClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		logger.Error("Failed to create dynamic Kubernetes client", "error", err)
		os.Exit(1)
	}

	discovery := k8s.NewDiscovery(kubeClient, logger)
	veleroClient := k8s.NewDynamicVeleroClient(dynClient)

	metrics := pvcguardian.NewMetrics()
	if cfg.Metrics.Enabled {
		logger.Info("Starting Prometheus metrics server", "addr", cfg.Metrics.Addr)
		if err := metrics.StartServer(cfg.Metrics.Addr); err != nil {
			logger.Error("Failed to start metrics server", "error", err)
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	reconciler := pvcguardian.NewReconciler(cfg, *configPath, discovery, veleroClient, nil, logger)

	if err := reconciler.RunLoop(ctx); err != nil && err != context.Canceled {
		logger.Error("Reconciler exited with error", "error", err)
		os.Exit(1)
	}

	logger.Info("PVCGuardian gracefully stopped")
}

func buildKubeConfig(kubeconfigPath string) (*rest.Config, error) {
	if kubeconfigPath != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	}
	cfg, err := rest.InClusterConfig()
	if err == nil {
		return cfg, nil
	}
	return clientcmd.BuildConfigFromFlags("", clientcmd.RecommendedHomeFile)
}
