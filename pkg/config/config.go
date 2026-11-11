// SPDX-License-Identifier: GPL-3.0-only
package config

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

var (
	cronFieldRegex = regexp.MustCompile(`^\S+\s+\S+\s+\S+\s+\S+\s+\S+$`)
	timeRegex      = regexp.MustCompile(`^([01]\d|2[0-3]):([0-5]\d)$`)
	retentionRegex = regexp.MustCompile(`^(\d+)([dhm])$`)
)

// TimeWindow defines an open-ended daily interval (e.g. from 20:00 to 23:00)
// during which a deterministic cron minute and hour will be scheduled.
type TimeWindow struct {
	From string `yaml:"from"` // HH:MM
	To   string `yaml:"to"`   // HH:MM
}

// WindowMinutes returns From and To parsed as minutes from midnight.
// If From and To are identical, an error is returned. If From > To, the
// window crosses midnight (e.g. 23:00 to 03:00).
func (w TimeWindow) WindowMinutes() (int, int, error) {
	mFrom, err := parseHHMM(w.From)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid 'from' time %q: %w", w.From, err)
	}
	mTo, err := parseHHMM(w.To)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid 'to' time %q: %w", w.To, err)
	}
	if mFrom == mTo {
		return 0, 0, fmt.Errorf("'from' (%s) and 'to' (%s) must not be equal", w.From, w.To)
	}
	return mFrom, mTo, nil
}

func parseHHMM(s string) (int, error) {
	matches := timeRegex.FindStringSubmatch(strings.TrimSpace(s))
	if len(matches) != 3 {
		return 0, fmt.Errorf("expected HH:MM (00:00 to 23:59)")
	}
	h, _ := strconv.Atoi(matches[1])
	m, _ := strconv.Atoi(matches[2])
	return h*60 + m, nil
}

// DiscoveryConfig controls how releases and PVCs are discovered.
type DiscoveryConfig struct {
	RequirePVC        bool     `yaml:"requirePVC"`        // default true
	DefaultPreset     string   `yaml:"defaultPreset"`     // default "evening"
	ExcludeNamespaces []string `yaml:"excludeNamespaces"` // default [kube-system]
}

// VeleroConfig holds Velero destination parameters.
type VeleroConfig struct {
	Namespace                string `yaml:"namespace"`       // default "velero"
	StorageLocation          string `yaml:"storageLocation"` // optional
	DefaultVolumesToFsBackup bool   `yaml:"defaultVolumesToFsBackup"`
	SnapshotVolumes          bool   `yaml:"snapshotVolumes"` // default false
}

// SchedulesConfig holds presets windows, custom crons, timezone and retention.
type SchedulesConfig struct {
	Timezone  string                `yaml:"timezone"`  // default "UTC"
	Retention string                `yaml:"retention"` // e.g. "5d", "120h"
	Windows   map[string]TimeWindow `yaml:"windows"`
	Cron      map[string]string     `yaml:"cron"`
}

// ReconcileConfig controls reconcile intervals.
type ReconcileConfig struct {
	Interval time.Duration `yaml:"interval"` // default 15m
}

// LoggingConfig holds log preferences.
type LoggingConfig struct {
	Level  string `yaml:"level"`  // "debug" | "info" | "warn" | "error"
	Format string `yaml:"format"` // "json" | "text"
}

// MetricsConfig holds prometheus endpoint settings.
type MetricsConfig struct {
	Enabled bool   `yaml:"enabled"`
	Addr    string `yaml:"addr"` // default ":8080"
}

// Config is the top-level configuration structure.
type Config struct {
	Discovery DiscoveryConfig `yaml:"discovery"`
	Velero    VeleroConfig    `yaml:"velero"`
	Schedules SchedulesConfig `yaml:"schedules"`
	Reconcile ReconcileConfig `yaml:"reconcile"`
	Logging   LoggingConfig   `yaml:"logging"`
	Metrics   MetricsConfig   `yaml:"metrics"`
}

// DefaultConfig returns a complete configuration with production defaults.
func DefaultConfig() Config {
	return Config{
		Discovery: DiscoveryConfig{
			RequirePVC:    true,
			DefaultPreset: "evening",
			ExcludeNamespaces: []string{
				"kube-system",
			},
		},
		Velero: VeleroConfig{
			Namespace:                "velero",
			StorageLocation:          "",
			DefaultVolumesToFsBackup: true,
			SnapshotVolumes:          false,
		},
		Schedules: SchedulesConfig{
			Timezone:  "UTC",
			Retention: "5d",
			Windows: map[string]TimeWindow{
				"day":     {From: "10:00", To: "16:00"},
				"evening": {From: "20:00", To: "23:00"},
				"night":   {From: "01:00", To: "04:00"},
			},
			Cron: make(map[string]string),
		},
		Reconcile: ReconcileConfig{
			Interval: 15 * time.Minute,
		},
		Logging: LoggingConfig{
			Level:  "info",
			Format: "json",
		},
		Metrics: MetricsConfig{
			Enabled: true,
			Addr:    ":8080",
		},
	}
}

// Load reads and parses a YAML configuration file, merging over defaults.
func Load(path string) (*Config, error) {
	cfg := DefaultConfig()
	if path == "" {
		return &cfg, nil
	}

	// The path is supplied by the operator through the --config flag, not by
	// an untrusted caller.
	data, err := os.ReadFile(path) // #nosec G304 -- operator-controlled config path.
	if err != nil {
		return nil, fmt.Errorf("reading config file %s: %w", path, err)
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("unmarshaling config YAML: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validating config: %w", err)
	}

	return &cfg, nil
}

// Validate ensures consistency of all configuration entries.
func (c *Config) Validate() error {
	// Validate defaultPreset exists in windows or custom cron
	if !c.HasPreset(c.Discovery.DefaultPreset) {
		return fmt.Errorf("discovery.defaultPreset %q is not defined in schedules.windows or schedules.cron", c.Discovery.DefaultPreset)
	}

	// Validate windows
	for name, win := range c.Schedules.Windows {
		if _, _, err := win.WindowMinutes(); err != nil {
			return fmt.Errorf("schedules.windows[%s]: %w", name, err)
		}
	}

	// Validate custom cron expressions (must be standard 5-part cron)
	for name, expr := range c.Schedules.Cron {
		if !cronFieldRegex.MatchString(strings.TrimSpace(expr)) {
			return fmt.Errorf("schedules.cron[%s]: invalid 5-field cron expression %q", name, expr)
		}
	}

	// Velero interprets cron expressions in UTC. The timezone option is
	// validated but no conversion is applied, so refuse anything but UTC
	// instead of silently ignoring the setting.
	if tz := strings.TrimSpace(c.Schedules.Timezone); tz != "" && !strings.EqualFold(tz, "UTC") {
		return fmt.Errorf("schedules.timezone: only UTC is supported (got %q)", c.Schedules.Timezone)
	}

	// Validate retention
	if _, err := ParseRetentionToTTL(c.Schedules.Retention); err != nil {
		return fmt.Errorf("schedules.retention: %w", err)
	}

	// Validate reconcile interval
	if c.Reconcile.Interval < 10*time.Second {
		return fmt.Errorf("reconcile.interval must be at least 10s (got %s)", c.Reconcile.Interval)
	}

	// Validate logging
	switch strings.ToLower(c.Logging.Level) {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("logging.level must be debug, info, warn or error (got %q)", c.Logging.Level)
	}
	switch strings.ToLower(c.Logging.Format) {
	case "json", "text":
	default:
		return fmt.Errorf("logging.format must be json or text (got %q)", c.Logging.Format)
	}

	return nil
}

// HasPreset checks if a preset exists in windows or custom cron.
func (c *Config) HasPreset(name string) bool {
	if _, ok := c.Schedules.Windows[name]; ok {
		return true
	}
	if _, ok := c.Schedules.Cron[name]; ok {
		return true
	}
	return false
}

// ParseRetentionToTTL converts "5d", "48h", "30m" to a time.Duration.
func ParseRetentionToTTL(retention string) (time.Duration, error) {
	retention = strings.TrimSpace(strings.ToLower(retention))
	matches := retentionRegex.FindStringSubmatch(retention)
	if len(matches) != 3 {
		return 0, fmt.Errorf("invalid retention format %q: expected format like 5d, 48h, 30m", retention)
	}
	val, _ := strconv.Atoi(matches[1])
	unit := matches[2]
	switch unit {
	case "d":
		return time.Duration(val) * 24 * time.Hour, nil
	case "h":
		return time.Duration(val) * time.Hour, nil
	case "m":
		return time.Duration(val) * time.Minute, nil
	default:
		return 0, fmt.Errorf("unsupported retention unit: %s", unit)
	}
}
