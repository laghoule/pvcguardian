# PVCGuardian

**PVCGuardian** is a Kubernetes daemon that discovers Helm releases and selector-declared backup units across scanned namespaces and automatically generates, maintains, and reconciles [Velero](https://velero.io) `Schedule` resources to back up their Persistent Volume Claims (PVCs) with deterministic execution windows.

---

## Key Features

- **Helm Workload Discovery**: Detects releases via standard Helm labels (`app.kubernetes.io/managed-by=Helm` and `app.kubernetes.io/instance`) across Deployments, StatefulSets, DaemonSets, CronJobs, and Jobs without requiring cluster-wide access to Kubernetes Secrets.
- **Opt-in Control**:
  - Scanning is cluster-wide by default across all namespaces (excluding `kube-system`).
  - Helm-managed workloads opt in by applying the label `pvcguardian.io/enabled: "true"` (Deployment, StatefulSet, DaemonSet, CronJob, Job). Non-Helm workloads opt in by declaring `pvcguardian.io/selector` instead (see below).
- **Selector-based Backup Units**: Any workload — Helm-managed or not — can declare its own backup scope with the annotation `pvcguardian.io/selector: "<key>=<value>[,<key>=<value>]"`. On a Helm-managed workload the selector takes precedence over the instance-based association. The selector is embedded directly in the Velero `Schedule`, so operator-managed PVCs are covered without a Helm release. Declaring the annotation implies opt-in. Only equality matching (`matchLabels`) is supported.
- **Custom Schedule Presets via Annotations**:
  - A backup unit can choose its schedule window by simply adding the annotation `pvcguardian.io/preset: "night"` on its workload, without altering the daemon's configuration.
- **Explicit PVC Association**: PVCs that cannot be linked to a workload (operator-managed, manually created) can be attached to a backup unit by labeling them `pvcguardian.io/release: <unit-name>`. Their Schedule then selects them directly.
- **Stateless Backup Unit Filtering**: `requirePVC: true` skips backup units that have no associated Persistent Volume Claims, preventing empty backup schedules.
- **Deterministic Time-Window Spreading**:
  - Presets define daily windows (`day`: 10:00-16:00, `evening`: 20:00-23:00, `night`: 01:00-04:00, all in UTC).
  - Each backup unit is assigned a stable, pseudo-random slot calculated from a SHA-256 hash of its namespace and unit name. Prevents backup thundering herds.
- **Zero-Data-Loss Lifecycle Management**:
  - Automatically deletes orphaned `Schedule` objects when a backup unit is removed or opted out.
  - **Never deletes existing `Backup` objects**; Velero TTL handles retention expiration safely.
- **Safe Truncated Naming**: Stable names up to 63 characters (`pvcguardian-<ns>-<release>` or `pvcguardian-<prefix>-<hash8>`). Safe renames execute create-before-delete.
- **Uncovered PVCs Detection**: Audits scanned namespaces for PVCs not matched to any backup unit (including PVCs of workloads that did not opt in), emitting Prometheus metrics and warning logs.
- **Observability**: Structured logs (`log/slog`), Prometheus metrics (`/metrics`), and a health probe (`/healthz`) served by the metrics listener when `metrics.enabled` is true.

---

## Configuration Example

```yaml
discovery:
  requirePVC: true                         # Skip releases without PVCs
  defaultPreset: evening                   # Default preset applied to opted-in releases
  excludeNamespaces:                       # Namespaces excluded from discovery
    - kube-system

velero:
  namespace: velero
  storageLocation: default           # Optional Velero BackupStorageLocation
  defaultVolumesToFsBackup: true     # Use Kopia filesystem backup (portable across all StorageClasses)

schedules:
  retention: 5d                      # Converted to Velero TTL (120h0m0s)
  # Windows are evaluated in UTC; Velero interprets cron expressions as UTC.
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

## Selector-based Backup Units

Annotate any workload — operator-created, plain manifest, or Helm-managed — to define its backup scope explicitly. On a Helm-managed workload, the selector overrides the instance-based association:

```yaml
metadata:
  annotations:
    pvcguardian.io/selector: "app.kubernetes.io/part-of=coroot"
    pvcguardian.io/preset: "night"        # optional
```

The unit name is `app.kubernetes.io/instance` when present, otherwise the workload name. The resulting `Schedule` keeps the declared selector and the explicit `pvcguardian.io/release=<unit>` selector, so PVCs matched by the selector or labelled explicitly are backed up together. Invalid or conflicting selectors are logged and ignored; the affected PVCs are reported as uncovered.

The PVC-level `pvcguardian.io/release: <unit>` label takes precedence over selector matching.

---

## Known Limitations

1. **Workload-less Helm Charts**: Releases that deploy only resources other than Deployments, StatefulSets, DaemonSets, CronJobs or Jobs (for example ConfigMaps, plain Secrets, or CRDs) are invisible to PVCGuardian. It discovers workload objects rather than reading Helm's release state from its Kubernetes Secrets (`sh.helm.release.v1.*`).
2. **Missing `app.kubernetes.io/instance` Label**: PVCs without the `instance` label are still associated with a Helm release when a workload's pod template references them (`persistentVolumeClaim.claimName`), when they follow a StatefulSet `volumeClaimTemplates` naming pattern, or when they carry an owner reference to a Helm-managed workload. For any remaining case (e.g. operator-managed PVCs), either declare a `pvcguardian.io/selector` annotation on the owning workload, or label the PVC with `pvcguardian.io/release: <release-name>` to attach it explicitly.

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
| `pvcguardian_schedules_unmanaged` | Gauge | Velero Schedules in the Velero namespace not managed by PVCGuardian |
| `pvcguardian_releases_skipped{reason="opt_out\|no_pvc"}` | Gauge | Backup units skipped during the last reconciliation cycle, by reason |
| `pvcguardian_releases_skipped_total{reason="opt_out\|no_pvc"}` | Counter | Cumulative skip decisions, for rates and alerts |
| `pvcguardian_uncovered_pvcs` | Gauge | Count of PVCs in scanned namespaces not covered by any backup unit |

---

## Development

```bash
# Run unit tests with race detection
make test

# Run static analysis
make lint

# Run gosec security scan
make gosec

# Compile static binary
make build

# Build container image
make image
```

---

## AI-assisted development

This project, its Helm chart, its CI/CD workflows, and this documentation were created with the help of an AI coding agent (OpenCode), under human review and direction. All Go code is validated with `go test -race` and `go vet`, and the Helm chart is linted with `helm lint` (including JSON Schema validation) in CI.

---

## License

This project is licensed under the GNU General Public License v3.0 (`GPL-3.0-only`). See [LICENSE](LICENSE) for the full license text.
