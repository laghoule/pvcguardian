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
	ReconcileOperationErrors *prometheus.CounterVec
	LastSuccessfulReconcile  prometheus.Gauge
	Leader                   prometheus.Gauge
	SchedulesManaged         prometheus.Gauge
	SchedulesUnmanaged       prometheus.Gauge
	ReleasesSkipped          *prometheus.GaugeVec
	ReleasesSkippedTotal     *prometheus.CounterVec
	UncoveredPVCs            prometheus.Gauge
	IndirectPVCs             prometheus.Gauge
	ConflictingUnits         prometheus.Gauge
	registry                 *prometheus.Registry
	server                   *http.Server
	healthChecker            func() bool
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
			[]string{"result"}, // "success" | "partial" | "error"
		),
		ReconcileDurationSeconds: prometheus.NewHistogram(
			prometheus.HistogramOpts{
				Name:    "pvcguardian_reconcile_duration_seconds",
				Help:    "Duration of reconciliation cycle in seconds.",
				Buckets: prometheus.DefBuckets,
			},
		),
		ReconcileOperationErrors: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "pvcguardian_reconcile_operation_errors_total",
				Help: "Total number of failed config reloads, discovery, build, or Velero schedule write operations.",
			},
			[]string{"operation"}, // "config_reload" | "discovery" | "build" | "create" | "update" | "delete" | "name_collision"
		),
		LastSuccessfulReconcile: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "pvcguardian_last_successful_reconcile_timestamp_seconds",
				Help: "Unix timestamp of the last fully successful reconciliation cycle.",
			},
		),
		Leader: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "pvcguardian_leader",
				Help: "1 when this replica holds the leader-election lease and reconciles Schedules, 0 for a standby replica.",
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
			[]string{"reason"}, // "opt_out" | "no_pvc" | "conflict"
		),
		ReleasesSkippedTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "pvcguardian_releases_skipped_total",
				Help: "Total number of skip decisions for backup units, partitioned by reason.",
			},
			[]string{"reason"}, // "opt_out" | "no_pvc" | "conflict"
		),
		UncoveredPVCs: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "pvcguardian_uncovered_pvcs",
				Help: "Number of PVCs in scanned namespaces that are not covered by any backup unit.",
			},
		),
		IndirectPVCs: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "pvcguardian_pvcs_not_directly_selected",
				Help: "Number of PVCs associated with a backup unit through workload references but not directly selectable by the generated Schedule label selectors.",
			},
		),
		ConflictingUnits: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "pvcguardian_conflicting_units",
				Help: "Number of backup units skipped during the last reconciliation cycle due to conflicting configurations (selectors, presets, or opt-in settings).",
			},
		),
	}

	reg.MustRegister(
		m.ReconcileTotal,
		m.ReconcileDurationSeconds,
		m.ReconcileOperationErrors,
		m.LastSuccessfulReconcile,
		m.Leader,
		m.SchedulesManaged,
		m.SchedulesUnmanaged,
		m.ReleasesSkipped,
		m.ReleasesSkippedTotal,
		m.UncoveredPVCs,
		m.IndirectPVCs,
		m.ConflictingUnits,
	)

	return m
}

// SetHealthChecker registers a callback to report daemon readiness for /readyz
// and the legacy /healthz endpoint.
func (m *Metrics) SetHealthChecker(checker func() bool) {
	m.healthChecker = checker
}

// SetLeader records whether this replica currently holds the leader lease.
func (m *Metrics) SetLeader(isLeader bool) {
	if isLeader {
		m.Leader.Set(1)
		return
	}
	m.Leader.Set(0)
}

// StartServer starts the HTTP server exposing Prometheus metrics on /metrics.
func (m *Metrics) StartServer(addr string) error {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{}))

	// /livez only proves that the process and HTTP server are alive. /readyz
	// (and the legacy /healthz) additionally require a recent successful
	// reconciliation while this replica is the leader, so a transient reconcile
	// failure does not restart a healthy process but does remove it from service
	// endpoints. Standby replicas are ready without reconciling; the process
	// exits if it loses the lease unexpectedly.
	mux.HandleFunc("/livez", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})

	readiness := func(w http.ResponseWriter, r *http.Request) {
		if m.healthChecker != nil && !m.healthChecker() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "unhealthy\n")
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	}
	mux.HandleFunc("/healthz", readiness)
	mux.HandleFunc("/readyz", readiness)

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

	outcome := OutcomeFailed
	if res != nil && res.Outcome != "" {
		outcome = res.Outcome
	} else if err == nil {
		outcome = OutcomeSuccess
	}
	m.ReconcileTotal.WithLabelValues(string(outcome)).Inc()

	if res == nil {
		return
	}

	m.ReconcileOperationErrors.WithLabelValues("config_reload").Add(float64(res.ConfigReloadErrors))
	m.ReconcileOperationErrors.WithLabelValues("discovery").Add(float64(res.DiscoveryErrors))
	m.ReconcileOperationErrors.WithLabelValues("build").Add(float64(res.BuildErrors))
	m.ReconcileOperationErrors.WithLabelValues("create").Add(float64(res.CreateErrors))
	m.ReconcileOperationErrors.WithLabelValues("update").Add(float64(res.UpdateErrors))
	m.ReconcileOperationErrors.WithLabelValues("delete").Add(float64(res.DeleteErrors))
	m.ReconcileOperationErrors.WithLabelValues("name_collision").Add(float64(res.NameCollisions))

	if outcome == OutcomeFailed {
		// The cycle could not establish a trustworthy cluster view. Keep the
		// last known gauges instead of publishing zeroes.
		return
	}

	m.SchedulesManaged.Set(float64(res.SchedulesManaged))
	m.SchedulesUnmanaged.Set(float64(res.SchedulesUnmanaged))
	m.UncoveredPVCs.Set(float64(res.UncoveredPVCs))
	m.IndirectPVCs.Set(float64(res.IndirectPVCs))
	m.ConflictingUnits.Set(float64(res.ConflictingUnits))

	// Gauges are set (not accumulated) so they reflect the last cycle and
	// do not depend on the reconcile interval. All reasons are always
	// written to avoid stale values.
	m.ReleasesSkipped.WithLabelValues("opt_out").Set(float64(res.SkippedOptOut))
	m.ReleasesSkipped.WithLabelValues("no_pvc").Set(float64(res.SkippedNoPVC))
	m.ReleasesSkipped.WithLabelValues("conflict").Set(float64(res.SkippedConflict))

	if res.SkippedOptOut > 0 {
		m.ReleasesSkippedTotal.WithLabelValues("opt_out").Add(float64(res.SkippedOptOut))
	}
	if res.SkippedNoPVC > 0 {
		m.ReleasesSkippedTotal.WithLabelValues("no_pvc").Add(float64(res.SkippedNoPVC))
	}
	if res.SkippedConflict > 0 {
		m.ReleasesSkippedTotal.WithLabelValues("conflict").Add(float64(res.SkippedConflict))
	}

	if outcome == OutcomeSuccess {
		m.LastSuccessfulReconcile.Set(float64(time.Now().Unix()))
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
