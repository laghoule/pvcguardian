// SPDX-License-Identifier: GPL-3.0-only
package pvcguardian

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"
)

func TestMetricsObserveAndServer(t *testing.T) {
	m := NewMetrics()

	// Observe a cycle
	res := &ReconcileResult{
		CreatedSchedules:   2,
		DeletedSchedules:   0,
		SchedulesManaged:   2,
		SchedulesUnmanaged: 1,
		SkippedOptOut:      1,
		SkippedNoPVC:       2,
		UncoveredPVCs:      3,
	}
	m.ObserveCycle(150*time.Millisecond, res, nil)

	// Start server on random local port
	addr := "127.0.0.1:18089"
	if err := m.StartServer(addr); err != nil {
		t.Fatalf("failed to start metrics server: %v", err)
	}
	defer func() {
		_ = m.StopServer(context.Background())
	}()

	// Allow server to bind
	time.Sleep(50 * time.Millisecond)

	// Test healthz
	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("healthz request failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "ok\n" {
		t.Errorf("unexpected healthz response: %s", string(body))
	}

	// Test /metrics
	respMetrics, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatalf("metrics request failed: %v", err)
	}
	metricsBody, _ := io.ReadAll(respMetrics.Body)
	_ = respMetrics.Body.Close()

	bodyStr := string(metricsBody)
	if !containsString(bodyStr, "pvcguardian_reconcile_total") {
		t.Errorf("expected pvcguardian_reconcile_total in metrics output")
	}
	if !containsString(bodyStr, "pvcguardian_schedules_managed 2") {
		t.Errorf("expected pvcguardian_schedules_managed 2 in metrics output")
	}
	if !containsString(bodyStr, "pvcguardian_schedules_unmanaged 1") {
		t.Errorf("expected pvcguardian_schedules_unmanaged 1 in metrics output")
	}
	if !containsString(bodyStr, "pvcguardian_uncovered_pvcs 3") {
		t.Errorf("expected pvcguardian_uncovered_pvcs 3 in metrics output")
	}
	if !containsString(bodyStr, `pvcguardian_releases_skipped{reason="opt_out"} 1`) {
		t.Errorf("expected pvcguardian_releases_skipped{reason=opt_out} 1 in metrics output")
	}
	if !containsString(bodyStr, `pvcguardian_releases_skipped{reason="no_pvc"} 2`) {
		t.Errorf("expected pvcguardian_releases_skipped{reason=no_pvc} 2 in metrics output")
	}
	if !containsString(bodyStr, `pvcguardian_releases_skipped_total{reason="opt_out"} 1`) {
		t.Errorf("expected pvcguardian_releases_skipped_total{reason=opt_out} 1 in metrics output")
	}
	if !containsString(bodyStr, `pvcguardian_releases_skipped_total{reason="no_pvc"} 2`) {
		t.Errorf("expected pvcguardian_releases_skipped_total{reason=no_pvc} 2 in metrics output")
	}
}

func containsString(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 || (len(s) > 0 && searchSubstring(s, substr)))
}

func searchSubstring(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
