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
	SchedulesUnmanaged       prometheus.Gauge
	ReleasesSkipped          *prometheus.GaugeVec
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
		SchedulesUnmanaged: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "pvcguardian_schedules_unmanaged",
				Help: "Current number of Velero Schedules in the Velero namespace not managed by PVCGuardian.",
			},
		),
		ReleasesSkipped: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "pvcguardian_releases_skipped",
				Help: "Number of backup units (Helm releases or selector units) skipped during the last reconciliation cycle, partitioned by reason.",
			},
			[]string{"reason"}, // "opt_out" | "no_pvc"
		),
		ReleasesSkippedTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "pvcguardian_releases_skipped_total",
				Help: "Total number of skip decisions for backup units, partitioned by reason.",
			},
			[]string{"reason"}, // "opt_out" | "no_pvc"
		),
		UncoveredPVCs: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "pvcguardian_uncovered_pvcs",
				Help: "Number of PVCs in scanned namespaces that are not covered by any backup unit.",
			},
		),
	}

	reg.MustRegister(
		m.ReconcileTotal,
		m.ReconcileDurationSeconds,
		m.SchedulesManaged,
		m.SchedulesUnmanaged,
		m.ReleasesSkipped,
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
		m.SchedulesUnmanaged.Set(float64(res.SchedulesUnmanaged))
		m.UncoveredPVCs.Set(float64(res.UncoveredPVCs))

		// Gauges are set (not accumulated) so they reflect the last cycle and
		// do not depend on the reconcile interval. Both reasons are always
		// written to avoid stale values.
		m.ReleasesSkipped.WithLabelValues("opt_out").Set(float64(res.SkippedOptOut))
		m.ReleasesSkipped.WithLabelValues("no_pvc").Set(float64(res.SkippedNoPVC))

		if res.SkippedOptOut > 0 {
			m.ReleasesSkippedTotal.WithLabelValues("opt_out").Add(float64(res.SkippedOptOut))
		}
		if res.SkippedNoPVC > 0 {
			m.ReleasesSkippedTotal.WithLabelValues("no_pvc").Add(float64(res.SkippedNoPVC))
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
