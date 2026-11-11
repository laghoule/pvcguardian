// SPDX-License-Identifier: GPL-3.0-only
package pvcguardian

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds Prometheus collectors for PVCGuardian.
type Metrics struct {
	ReconcileTotal           *prometheus.CounterVec
	ReconcileDurationSeconds prometheus.Histogram
	SchedulesManaged         prometheus.Gauge
	ReleasesSkippedTotal     *prometheus.CounterVec
	UncoveredPVCs            prometheus.Gauge
	registry                 *prometheus.Registry
	server                   *http.Server
}

// NewMetrics initializes and registers all Prometheus collectors.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()

	m := &Metrics{
		registry: reg,
		ReconcileTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "pvcguardian_reconcile_total",
				Help: "Total number of reconciliation cycles completed, partitioned by result.",
			},
			[]string{"result"}, // "success" | "error"
		),
		ReconcileDurationSeconds: prometheus.NewHistogram(
			prometheus.HistogramOpts{
				Name:    "pvcguardian_reconcile_duration_seconds",
				Help:    "Duration of reconciliation cycle in seconds.",
				Buckets: prometheus.DefBuckets,
			},
		),
		SchedulesManaged: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "pvcguardian_schedules_managed",
				Help: "Current number of Velero Schedules actively maintained by PVCGuardian.",
			},
		),
		ReleasesSkippedTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "pvcguardian_releases_skipped_total",
				Help: "Total number of releases skipped during discovery.",
			},
			[]string{"reason"}, // "no_pvc" | "opt_out"
		),
		UncoveredPVCs: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "pvcguardian_uncovered_pvcs",
				Help: "Number of PVCs in opt-in namespaces that are not covered by any backup unit.",
			},
		),
	}

	reg.MustRegister(
		m.ReconcileTotal,
		m.ReconcileDurationSeconds,
		m.SchedulesManaged,
		m.ReleasesSkippedTotal,
		m.UncoveredPVCs,
	)

	return m
}

// StartServer starts the HTTP server exposing Prometheus metrics on /metrics.
func (m *Metrics) StartServer(addr string) error {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})

	m.server = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		if err := m.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("Metrics HTTP server failed", "error", err)
		}
	}()

	return nil
}

// StopServer gracefully stops the metrics server.
func (m *Metrics) StopServer(ctx context.Context) error {
	if m.server == nil {
		return nil
	}
	return m.server.Shutdown(ctx)
}

// ObserveCycle updates Prometheus gauges and histograms after a reconcile pass.
func (m *Metrics) ObserveCycle(duration time.Duration, res *ReconcileResult, err error) {
	m.ReconcileDurationSeconds.Observe(duration.Seconds())
	if err != nil {
		m.ReconcileTotal.WithLabelValues("error").Inc()
		return
	}
	m.ReconcileTotal.WithLabelValues("success").Inc()

	if res != nil {
		m.SchedulesManaged.Set(float64(res.SchedulesManaged))
		m.UncoveredPVCs.Set(float64(res.UncoveredPVCs))
		if res.SkippedReleases > 0 {
			m.ReleasesSkippedTotal.WithLabelValues("policy").Add(float64(res.SkippedReleases))
		}
	}
}

// SetupLogger initializes a structured slog.Logger based on configuration.
func SetupLogger(levelStr, formatStr string) *slog.Logger {
	var level slog.Level
	switch strings.ToLower(levelStr) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{
		Level: level,
	}

	var handler slog.Handler
	if strings.ToLower(formatStr) == "json" {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}

	logger := slog.New(handler)
	slog.SetDefault(logger)
	return logger
}
