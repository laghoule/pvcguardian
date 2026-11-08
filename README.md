# PVCGuardian

**PVCGuardian** is a Kubernetes daemon that discovers Helm releases in opt-in namespaces and automatically generates, maintains, and reconciles [Velero](https://velero.io) `Schedule` resources to back up their Persistent Volume Claims (PVCs) with deterministic execution windows.

---

## Key Features

- **Helm Workload Discovery**: Detects releases via standard Helm labels (`app.kubernetes.io/managed-by=Helm` and `app.kubernetes.io/instance`) across Deployments, StatefulSets, DaemonSets, CronJobs, and Jobs without requiring cluster-wide access to Kubernetes Secrets.
- **Opt-in Control**:
  - Namespace-level: label `pvcguardian.io/enabled: "true"` enables all releases in that namespace.
  - Release-level override: label `pvcguardian.io/enabled: "false"` on any workload explicitly opts out that release.
- **Stateless Release Filtering**: `requirePVC: true` skips releases that have no associated Persistent Volume Claims, preventing empty backup schedules.
- **Deterministic Time-Window Spreading**:
  - Presets define daily windows (`day`: 10:00-16:00, `evening`: 20:00-23:00, `night`: 01:00-04:00, all in UTC).
  - Each release is assigned a stable, pseudo-random slot calculated from a SHA-256 hash of its namespace and release name. Prevents backup thundering herds.
- **Zero-Data-Loss Lifecycle Management**:
  - Automatically deletes orphaned `Schedule` objects when a release is uninstalled or opted out.
  - **Never deletes existing `Backup` objects**; Velero TTL handles retention expiration safely.
- **Safe Truncated Naming**: Stable names up to 63 characters (`pvcguardian-<ns>-<release>` or `pvcguardian-<prefix>-<hash8>`). Safe renames execute create-before-delete.
- **Uncovered PVCs Detection**: Audits namespaces for manual or unmanaged PVCs, emitting Prometheus metrics and warning logs.
- **Observability**: Structured logs (`log/slog`), health probe (`/healthz`), and Prometheus metrics (`/metrics`).

---

## Configuration Example

```yaml
selectors:
  label: pvcguardian.io/enabled      # Opt-in label key (expects "true" or "false")
  requirePVC: true                   # Skip releases without PVCs
  defaultPreset: evening             # Default preset applied to releases
  namespaces:                        # Per-namespace preset override
    staging: day
    prod: evening
  releases:                          # Per-release override
    - namespace: prod
      name: critical-database
      preset: night

velero:
  namespace: velero
  storageLocation: default           # Optional Velero BackupStorageLocation
  defaultVolumesToFsBackup: true     # Use Kopia filesystem backup (portable across all StorageClasses)

schedules:
  timezone: UTC
  retention: 5d                      # Converted to Velero TTL (120h0m0s)
  windows:
    day:
      from: "10:00"
      to: "16:00"
    evening:
      from: "20:00"
      to: "23:00"
    night:
      from: "01:00"
      to: "04:00"
  cron:
    weekly: "0 3 * * 0"              # Custom 5-field cron presets

reconcile:
  interval: 15m                      # Periodic reconciliation loop interval

logging:
  level: info                        # debug | info | warn | error
  format: json                       # json | text

metrics:
  enabled: true
  addr: ":8080"                      # Exposes /metrics and /healthz
```

---

## Known Limitations

1. **Workload-less Helm Charts**: Releases that deploy only ConfigMaps, Secrets, or CRDs (without any Deployment, StatefulSet, DaemonSet, CronJob, or Job) are invisible to PVCGuardian since it does not inspect Helm Secrets.
2. **Missing `app.kubernetes.io/instance` Label**: PVCs or workloads created by non-standard charts lacking the `instance` label cannot be associated with a release and will be flagged as uncovered PVCs.

---

## Deployment with Helm

PVCGuardian provides an official Helm chart under `deploy/chart/pvcguardian`:

```bash
helm install pvcguardian deploy/chart/pvcguardian \
  --namespace pvcguardian \
  --create-namespace \
  -f my-values.yaml
```

---

## Prometheus Metrics

| Metric | Type | Description |
|---|---|---|
| `pvcguardian_reconcile_total{result="success\|error"}` | Counter | Total number of reconciliation passes completed |
| `pvcguardian_reconcile_duration_seconds` | Histogram | Latency histogram for reconciliation passes |
| `pvcguardian_schedules_managed` | Gauge | Number of Velero Schedules currently maintained |
| `pvcguardian_releases_skipped_total{reason="policy"}` | Counter | Number of releases skipped (no PVC or opted out) |
| `pvcguardian_uncovered_pvcs` | Gauge | Count of PVCs in opt-in namespaces not covered by any Helm release |
| `pvcguardian_unlabeled_helm_workloads` | Counter | Workloads with `managed-by=Helm` but missing `instance` label |

---

## Development

```bash
# Run unit tests with race detection
make test

# Run static analysis
make lint

# Compile static binary
make build

# Build container image
make image
```

---

## License

This project is licensed under the GNU General Public License v3.0 (`GPL-3.0-only`). See [LICENSE](LICENSE) for the full license text.
