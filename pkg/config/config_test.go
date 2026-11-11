// SPDX-License-Identifier: GPL-3.0-only
package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("DefaultConfig should be valid: %v", err)
	}
	if !cfg.Discovery.RequirePVC {
		t.Errorf("expected requirePVC to default to true")
	}
	if cfg.Discovery.DefaultPreset != "evening" {
		t.Errorf("expected defaultPreset evening, got %s", cfg.Discovery.DefaultPreset)
	}
	if cfg.Schedules.Timezone != "UTC" {
		t.Errorf("expected timezone UTC, got %s", cfg.Schedules.Timezone)
	}
}

func TestParseRetentionToTTL(t *testing.T) {
	cases := []struct {
		input    string
		expected time.Duration
		err      bool
	}{
		{"5d", 5 * 24 * time.Hour, false},
		{"120h", 120 * time.Hour, false},
		{"30m", 30 * time.Minute, false},
		{"invalid", 0, true},
		{"5x", 0, true},
		{"-5d", 0, true},
	}
	for _, tc := range cases {
		ttl, err := ParseRetentionToTTL(tc.input)
		if tc.err && err == nil {
			t.Errorf("input %s: expected error, got nil", tc.input)
		}
		if !tc.err && err != nil {
			t.Errorf("input %s: unexpected error: %v", tc.input, err)
		}
		if !tc.err && ttl != tc.expected {
			t.Errorf("input %s: expected %v, got %v", tc.input, tc.expected, ttl)
		}
	}
}

func TestLoadValidYaml(t *testing.T) {
	tmp := t.TempDir()
	cfgFile := filepath.Join(tmp, "config.yaml")

	yamlContent := `
discovery:
  requirePVC: false
  defaultPreset: day
  excludeNamespaces:
    - kube-system
    - custom-ignore

velero:
  namespace: velero-custom
  storageLocation: s3-backups
  defaultVolumesToFsBackup: true

schedules:
  timezone: UTC
  retention: 7d
  windows:
    day:
      from: "09:00"
      to: "12:00"
    evening:
      from: "20:00"
      to: "23:00"
    night:
      from: "01:00"
      to: "04:00"
  cron:
    weekly: "0 4 * * 0"

reconcile:
  interval: 30m

logging:
  level: debug
  format: text

metrics:
  enabled: true
  addr: ":9090"
`
	if err := os.WriteFile(cfgFile, []byte(yamlContent), 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	cfg, err := Load(cfgFile)
	if err != nil {
		t.Fatalf("Load returned unexpected error: %v", err)
	}

	if cfg.Discovery.RequirePVC != false {
		t.Errorf("expected requirePVC false")
	}
	if cfg.Velero.StorageLocation != "s3-backups" {
		t.Errorf("expected s3-backups storage location")
	}
	if cfg.Schedules.Retention != "7d" {
		t.Errorf("expected 7d retention")
	}
	if cfg.Reconcile.Interval != 30*time.Minute {
		t.Errorf("expected 30m interval")
	}
}

func TestInvalidConfigFailsValidation(t *testing.T) {
	cases := []struct {
		name    string
		mutator func(c *Config)
	}{
		{
			name: "unknown defaultPreset",
			mutator: func(c *Config) {
				c.Discovery.DefaultPreset = "nonexistent"
			},
		},
		{
			name: "invalid window from == to",
			mutator: func(c *Config) {
				c.Schedules.Windows["bad"] = TimeWindow{From: "20:00", To: "20:00"}
			},
		},
		{
			name: "invalid cron expression",
			mutator: func(c *Config) {
				c.Schedules.Cron["bad"] = "not a cron"
			},
		},
		{
			name: "non-UTC timezone",
			mutator: func(c *Config) {
				c.Schedules.Timezone = "Europe/Paris"
			},
		},
		{
			name: "interval too short",
			mutator: func(c *Config) {
				c.Reconcile.Interval = 2 * time.Second
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			tc.mutator(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatalf("expected validation error for %s, got nil", tc.name)
			}
		})
	}
}
