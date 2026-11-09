// SPDX-License-Identifier: GPL-3.0-only
package pvcguardian

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/laghoule/pvcguardian/pkg/config"
	"github.com/laghoule/pvcguardian/pkg/k8s"
)

const (
	ManagedByLabel      = "app.kubernetes.io/managed-by"
	ManagedByValue      = "pvcguardian"
	SourceNamespaceAnno = "pvcguardian.io/source-namespace"
	SourceReleaseAnno   = "pvcguardian.io/source-release"
	SourceChartAnno     = "pvcguardian.io/source-chart"
	SlotAnno            = "pvcguardian.io/slot"
	PresetAnno          = "pvcguardian.io/preset"
	FullNameAnno        = "pvcguardian.io/full-name"
	NameHashLabel       = "pvcguardian.io/name-hash"

	SchedulePrefix = "pvcguardian-"
	MaxK8sNameLen  = 63
	HashSuffixLen  = 8
)

// ScheduleBuilder handles deterministic slot calculation, naming, and Velero Schedule construction.
type ScheduleBuilder struct {
	cfg *config.Config
}

// NewScheduleBuilder creates a builder using the given configuration.
func NewScheduleBuilder(cfg *config.Config) *ScheduleBuilder {
	return &ScheduleBuilder{cfg: cfg}
}

// DeterministicHash generates a sha256 hash string for a namespace and release pair.
func DeterministicHash(namespace, release string) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s/%s", namespace, release)))
	return hex.EncodeToString(h[:])
}

// ScheduleName formats a stable, truncated Kubernetes object name (<= 63 chars).
// Format: pvcguardian-<namespace>-<release>, or pvcguardian-<prefix-trunc>-<hash8>
func ScheduleName(namespace, release string) string {
	raw := fmt.Sprintf("%s%s-%s", SchedulePrefix, namespace, release)
	// Sanitize to valid k8s name chars
	raw = strings.ToLower(raw)
	raw = sanitizeK8sName(raw)

	if len(raw) <= MaxK8sNameLen {
		return raw
	}

	hash8 := DeterministicHash(namespace, release)[:HashSuffixLen]
	// Max length for prefix is 63 - 1 (dash) - 8 (hash) = 54
	maxPrefix := MaxK8sNameLen - HashSuffixLen - 1
	prefix := strings.TrimRight(raw[:maxPrefix], "-")
	return fmt.Sprintf("%s-%s", prefix, hash8)
}

func sanitizeK8sName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	return b.String()
}

// ResolvePreset determines the preset name for a release following the precedence:
// 1. Release workload annotation (pvcguardian.io/preset)
// 2. Namespace preset configuration
// 3. Default preset
func (b *ScheduleBuilder) ResolvePreset(namespace, annotatedPreset string) string {
	if annotatedPreset != "" && b.cfg.HasPreset(annotatedPreset) {
		return annotatedPreset
	}
	if p, ok := b.cfg.Selectors.Namespaces[namespace]; ok && p != "" {
		return p
	}
	return b.cfg.Selectors.DefaultPreset
}

// CalculateCron computes the deterministic cron expression (and slot representation)
// for a release and preset.
func (b *ScheduleBuilder) CalculateCron(namespace, release, preset string) (cronExpr string, slot string, err error) {
	if expr, ok := b.cfg.Schedules.Cron[preset]; ok {
		return expr, "custom", nil
	}

	win, ok := b.cfg.Schedules.Windows[preset]
	if !ok {
		return "", "", fmt.Errorf("preset %q not found in windows or cron", preset)
	}

	mFrom, mTo, err := win.WindowMinutes()
	if err != nil {
		return "", "", fmt.Errorf("parsing window for preset %q: %w", preset, err)
	}

	windowDuration := mTo - mFrom
	if windowDuration <= 0 {
		return "", "", fmt.Errorf("window %q has non-positive duration", preset)
	}

	hash := DeterministicHash(namespace, release)
	var hashNum uint32
	for i := 0; i < 8; i++ {
		hashNum = (hashNum << 4) | fromHex(hash[i])
	}

	offset := int(hashNum % uint32(windowDuration))
	targetMinuteOfDay := mFrom + offset

	hour := targetMinuteOfDay / 60
	minute := targetMinuteOfDay % 60

	cronExpr = fmt.Sprintf("%d %d * * *", minute, hour)
	slot = fmt.Sprintf("%02d:%02d", hour, minute)
	return cronExpr, slot, nil
}

func fromHex(c byte) uint32 {
	if c >= '0' && c <= '9' {
		return uint32(c - '0')
	}
	if c >= 'a' && c <= 'f' {
		return uint32(c - 'a' + 10)
	}
	return 0
}

// BuildSchedule constructs a complete Velero Schedule CR.
func (b *ScheduleBuilder) BuildSchedule(rel k8s.DiscoveredRelease) (*velerov1.Schedule, error) {
	preset := b.ResolvePreset(rel.Namespace, rel.AnnotatedPreset)
	cronExpr, slot, err := b.CalculateCron(rel.Namespace, rel.Name, preset)
	if err != nil {
		return nil, fmt.Errorf("calculating cron for %s/%s: %w", rel.Namespace, rel.Name, err)
	}

	ttl, err := config.ParseRetentionToTTL(b.cfg.Schedules.Retention)
	if err != nil {
		return nil, fmt.Errorf("parsing retention TTL: %w", err)
	}

	name := ScheduleName(rel.Namespace, rel.Name)
	fullName := fmt.Sprintf("%s%s-%s", SchedulePrefix, rel.Namespace, rel.Name)
	hash8 := DeterministicHash(rel.Namespace, rel.Name)[:HashSuffixLen]

	snapshotVolumes := false
	defaultFsBackup := b.cfg.Velero.DefaultVolumesToFsBackup

	sch := &velerov1.Schedule{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "velero.io/v1",
			Kind:       "Schedule",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: b.cfg.Velero.Namespace,
			Labels: map[string]string{
				ManagedByLabel: ManagedByValue,
				NameHashLabel:  hash8,
			},
			Annotations: map[string]string{
				FullNameAnno:        fullName,
				SourceNamespaceAnno: rel.Namespace,
				SourceReleaseAnno:   rel.Name,
				SourceChartAnno:     rel.Chart,
				PresetAnno:          preset,
				SlotAnno:            slot,
			},
		},
		Spec: velerov1.ScheduleSpec{
			Schedule: cronExpr,
			Template: velerov1.BackupSpec{
				IncludedNamespaces: []string{rel.Namespace},
				OrLabelSelectors: []*metav1.LabelSelector{
					{
						MatchLabels: map[string]string{
							k8s.HelmInstanceLabel: rel.Name,
						},
					},
				},
				TTL:                      metav1.Duration{Duration: ttl},
				DefaultVolumesToFsBackup: &defaultFsBackup,
				SnapshotVolumes:          &snapshotVolumes,
			},
		},
	}

	if b.cfg.Velero.StorageLocation != "" {
		sch.Spec.Template.StorageLocation = b.cfg.Velero.StorageLocation
	}

	return sch, nil
}
