// SPDX-License-Identifier: GPL-3.0-only
package pvcguardian

import (
	"strings"
	"testing"
	"time"

	"github.com/laghoule/pvcguardian/pkg/config"
	"github.com/laghoule/pvcguardian/pkg/k8s"
)

func TestDeterministicHashAndName(t *testing.T) {
	// Stable hash test
	h1 := DeterministicHash("prod", "my-app")
	h2 := DeterministicHash("prod", "my-app")
	if h1 != h2 {
		t.Fatalf("hashes should be identical: %s vs %s", h1, h2)
	}

	h3 := DeterministicHash("prod", "other-app")
	if h1 == h3 {
		t.Fatalf("different apps should have different hashes")
	}

	// Short name test
	shortName := ScheduleName("prod", "web")
	expectedShort := "pvcguardian-prod-web"
	if shortName != expectedShort {
		t.Errorf("expected %s, got %s", expectedShort, shortName)
	}

	// Long name test (must truncate to <= 63 chars and end with hash8)
	longRelease := "very-long-release-name-that-definitely-exceeds-the-maximum-sixty-three-character-limit-of-kubernetes-names"
	longName := ScheduleName("production-eu-west-1", longRelease)
	if len(longName) > MaxK8sNameLen {
		t.Errorf("name length %d exceeds max %d: %s", len(longName), MaxK8sNameLen, longName)
	}
	hash8 := DeterministicHash("production-eu-west-1", longRelease)[:HashSuffixLen]
	if !strings.HasSuffix(longName, hash8) {
		t.Errorf("long name %s should end with hash8 %s", longName, hash8)
	}
}

func TestResolvePreset(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Selectors.DefaultPreset = "evening"
	cfg.Selectors.Namespaces["staging"] = "day"
	cfg.Selectors.Releases = []config.ReleaseOverride{
		{Namespace: "staging", Name: "critical-db", Preset: "night"},
	}

	builder := NewScheduleBuilder(&cfg)

	// Release override
	if p := builder.ResolvePreset("staging", "critical-db"); p != "night" {
		t.Errorf("expected night, got %s", p)
	}
	// Namespace preset
	if p := builder.ResolvePreset("staging", "other-app"); p != "day" {
		t.Errorf("expected day, got %s", p)
	}
	// Default preset
	if p := builder.ResolvePreset("prod", "any-app"); p != "evening" {
		t.Errorf("expected evening, got %s", p)
	}
}

func TestCalculateCronWithinWindow(t *testing.T) {
	cfg := config.DefaultConfig()
	builder := NewScheduleBuilder(&cfg)

	// Test evening window: 20:00 to 23:00 (180 minutes)
	cron, slot, err := builder.CalculateCron("prod", "api-gateway", "evening")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Check slot format HH:MM
	if len(slot) != 5 || slot[2] != ':' {
		t.Errorf("invalid slot format: %s", slot)
	}

	// Check hour is in [20, 22] (since window is [20:00, 23:00))
	hour := int(slot[0]-'0')*10 + int(slot[1]-'0')
	if hour < 20 || hour >= 23 {
		t.Errorf("hour %d out of evening bounds [20, 23)", hour)
	}

	// Same inputs must yield same cron
	cron2, slot2, _ := builder.CalculateCron("prod", "api-gateway", "evening")
	if cron != cron2 || slot != slot2 {
		t.Errorf("deterministic cron calculation mismatch: %s vs %s", cron, cron2)
	}

	// Test custom cron
	cfg.Schedules.Cron["custom-weekly"] = "15 3 * * 1"
	cronCustom, slotCustom, err := builder.CalculateCron("prod", "app", "custom-weekly")
	if err != nil {
		t.Fatalf("unexpected error for custom cron: %v", err)
	}
	if cronCustom != "15 3 * * 1" || slotCustom != "custom" {
		t.Errorf("unexpected custom cron result: %s, %s", cronCustom, slotCustom)
	}
}

func TestBuildSchedule(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Velero.Namespace = "velero-backup"
	cfg.Velero.StorageLocation = "minio"
	cfg.Schedules.Retention = "10d"

	builder := NewScheduleBuilder(&cfg)

	rel := k8s.DiscoveredRelease{
		Namespace: "prod",
		Name:      "web-server",
		Chart:     "web-server-2.1.0",
		PVCs:      []string{"data-vol"},
	}

	sch, err := builder.BuildSchedule(rel)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if sch.Namespace != "velero-backup" {
		t.Errorf("expected namespace velero-backup, got %s", sch.Namespace)
	}
	if sch.Name != "pvcguardian-prod-web-server" {
		t.Errorf("expected name pvcguardian-prod-web-server, got %s", sch.Name)
	}
	if sch.Labels[ManagedByLabel] != ManagedByValue {
		t.Errorf("missing managed-by label")
	}
	if sch.Annotations[SourceNamespaceAnno] != "prod" {
		t.Errorf("expected source namespace prod")
	}
	if sch.Annotations[SourceReleaseAnno] != "web-server" {
		t.Errorf("expected source release web-server")
	}
	if sch.Annotations[SourceChartAnno] != "web-server-2.1.0" {
		t.Errorf("expected source chart web-server-2.1.0")
	}

	// Check BackupSpec
	expectedTTL := 10 * 24 * time.Hour
	if sch.Spec.Template.TTL.Duration != expectedTTL {
		t.Errorf("expected TTL %v, got %v", expectedTTL, sch.Spec.Template.TTL.Duration)
	}
	if len(sch.Spec.Template.IncludedNamespaces) != 1 || sch.Spec.Template.IncludedNamespaces[0] != "prod" {
		t.Errorf("expected includedNamespaces [prod], got %v", sch.Spec.Template.IncludedNamespaces)
	}
	if len(sch.Spec.Template.OrLabelSelectors) != 1 || sch.Spec.Template.OrLabelSelectors[0].MatchLabels[k8s.HelmInstanceLabel] != "web-server" {
		t.Errorf("expected orLabelSelectors instance=web-server")
	}
	if *sch.Spec.Template.DefaultVolumesToFsBackup != true {
		t.Errorf("expected DefaultVolumesToFsBackup to be true")
	}
	if *sch.Spec.Template.SnapshotVolumes != false {
		t.Errorf("expected SnapshotVolumes to be false")
	}
	if sch.Spec.Template.StorageLocation != "minio" {
		t.Errorf("expected storage location minio, got %s", sch.Spec.Template.StorageLocation)
	}
}
