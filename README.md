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
- **Backup-Preserving Lifecycle Management**:
  - Automatically deletes orphaned `Schedule` objects when a backup unit is removed, opted out, or its namespace is deleted or excluded.
  - **Never deletes existing `Backup` objects**; Velero TTL handles retention expiration safely.
  - Preserving `Backup` objects is not a data-loss guarantee: PVCGuardian does not verify that a Backup completed, is application-consistent, or can be restored. Those properties must be validated with restore drills.
- **Collision-safe Naming**: Stable names up to 63 characters that always include a hash of the backup unit identity (`pvcguardian-<ns>-<release>-<hash8>`, truncated when needed). Sanitized or ambiguous identities cannot silently collide. Safe renames execute create-before-delete.
- **Leader Election**: Replicas elect a single active reconciler through a Kubernetes `Lease`, so the Deployment can be scaled or rolled without concurrent writers. The lease name and namespace inherit from the Deployment and its installation namespace; timings and identity are constants. There is no leader-election configuration.
- **Uncovered PVCs Detection**: Audits scanned namespaces for PVCs not matched to any backup unit (including PVCs of workloads that did not opt in), emitting Prometheus metrics and warning logs. It also reports PVCs whose association relies on workload references rather than direct Schedule selectors.
- **Observability**: Structured logs (`log/slog`), Prometheus metrics (`/metrics`), a liveness endpoint (`/livez`), and readiness endpoints (`/readyz`, legacy `/healthz`) served by the metrics listener when `metrics.enabled` is true. A cycle is only considered successful when every discovery and Schedule write operation succeeded.

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
  defaultVolumesToFsBackup: true     # Use Kopia/Restic filesystem backup (requires Velero node-agent)
  snapshotVolumes: false             # Use CSI volume snapshots (default: false)

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
  addr: ":8080"                      # Exposes /metrics, /livez, /readyz, /healthz
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

The PVC-level `pvcguardian.io/release: <unit>` label takes precedence during discovery.

---

## PVC Coverage and Schedule Selectors

Discovery associates PVCs in several ways, but the generated Velero `Schedule` can only express label selectors:

| Association | Directly selected by the Schedule? |
|---|---|
| `pvcguardian.io/release=<unit>` on the PVC | Yes |
| `pvcguardian.io/selector` matchLabels on the PVC | Yes, for selector-based units |
| `app.kubernetes.io/instance=<unit>` on the PVC | Yes, for Helm-derived units |
| PVC referenced by a workload pod template, StatefulSet `volumeClaimTemplates`, or owner reference | Not necessarily; Velero's additional-items handling may include it when the workload is backed up |

PVCs in the last row are still associated with the unit (so `requirePVC` does not drop a useful backup unit), but they are counted by `pvcguardian_pvcs_not_directly_selected` and logged as warnings. Treat that metric as a to-do list: label the PVC with `pvcguardian.io/release=<unit>` to make its coverage explicit, and validate restoration end to end.

Explicit associations are not exclusive at the Velero level: Velero ORs all selectors in `orLabelSelectors`. A PVC labelled `pvcguardian.io/release=B` that also matches unit A's selector can therefore appear in both Schedules. Discovery reports this in a warning; avoid overlapping selectors or explicit labels between units when duplicate backups matter.

---

## Leader Election

All replicas participate in leader election through a `coordination.k8s.io/v1`
`Lease`. The lease is named after the Deployment (`PVCGUARDIAN_LEASE_NAME`) and
lives in the namespace where PVCGuardian is installed (`POD_NAMESPACE`). Only the
lease holder runs the reconciliation loop; the other replicas stay alive and
ready to take over. Two installations therefore never compete for the same lease,
even when they target the same Velero namespace.

There is deliberately **no user-facing configuration** for leader election. The
Helm chart injects the Deployment name and installation namespace automatically;
the remaining settings are constants in `pkg/pvcguardian/leader.go`:

| Setting | Value |
|---|---|
| Lease name | Deployment name (default `pvcguardian` outside Helm) |
| Lease namespace | PVCGuardian installation namespace (from the pod namespace) |
| Lease duration | 15 s |
| Renew deadline | 10 s |
| Retry period | 2 s |
| Identity | Pod hostname |

A replica that loses the lease unexpectedly exits with a non-zero status, so
Kubernetes restarts it and it re-joins the election. A graceful shutdown releases
the lease immediately. `/readyz` stays `200` on a standby replica (it can take
over) and requires a recent successful reconciliation on the leader. The
`pvcguardian_leader` gauge reports which replica is active.

The Helm chart creates a dedicated Role and RoleBinding for `leases`
(`get`, `create`, `update`) in the release namespace. No additional configuration
or namespace is required. Leader election does not guarantee strict fencing:
during failover, two replicas can briefly overlap. Schedule writes are
declarative and idempotent, so an overlap results in redundant writes at worst.

---

## Known Limitations

1. **Workload-less Helm Charts**: Releases that deploy only resources other than Deployments, StatefulSets, DaemonSets, CronJobs or Jobs (for example ConfigMaps, plain Secrets, or CRDs) are invisible to PVCGuardian. It discovers workload objects rather than reading Helm's release state from its Kubernetes Secrets (`sh.helm.release.v1.*`).
2. **Missing `app.kubernetes.io/instance` Label**: PVCs without the `instance` label are still associated with a Helm release when a workload's pod template references them (`persistentVolumeClaim.claimName`), when they follow a StatefulSet `volumeClaimTemplates` naming pattern, or when they carry an owner reference to a Helm-managed workload. This association is not directly selectable by the Schedule and depends on Velero's additional-items behaviour, so it is exposed as `pvcguardian_pvcs_not_directly_selected`. For a directly guaranteed scope, label the PVC with `pvcguardian.io/release: <release-name>` or declare a `pvcguardian.io/selector` annotation on the owning workload.
3. **No Built-in Restore Verification**: Unit tests validate the generated API objects, not the data path. Backups produced by PVCGuardian must be validated with a real Velero installation and a restore drill before they are relied on, especially for volumes covered through the indirect associations above.

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
| `pvcguardian_reconcile_total{result="success\|partial\|error"}` | Counter | Total reconciliation passes: fully successful, completed with at least one failure, or unable to establish a cluster view |
| `pvcguardian_reconcile_duration_seconds` | Histogram | Latency histogram for reconciliation passes |
| `pvcguardian_reconcile_operation_errors_total{operation="config_reload\|discovery\|build\|create\|update\|delete\|name_collision"}` | Counter | Failed config reloads, discovery, Schedule builds, and Velero writes |
| `pvcguardian_last_successful_reconcile_timestamp_seconds` | Gauge | Unix timestamp of the last fully successful reconciliation |
| `pvcguardian_leader` | Gauge | `1` on the replica holding the leader-election lease, `0` on standbys |
| `pvcguardian_schedules_managed` | Gauge | Number of Velero Schedules currently maintained |
| `pvcguardian_schedules_unmanaged` | Gauge | Velero Schedules in the Velero namespace not managed by PVCGuardian |
| `pvcguardian_releases_skipped{reason="opt_out\|no_pvc\|conflict"}` | Gauge | Backup units skipped during the last reconciliation cycle, by reason |
| `pvcguardian_releases_skipped_total{reason="opt_out\|no_pvc\|conflict"}` | Counter | Cumulative skip decisions, for rates and alerts |
| `pvcguardian_uncovered_pvcs` | Gauge | Count of PVCs in scanned namespaces not associated with any backup unit |
| `pvcguardian_pvcs_not_directly_selected` | Gauge | Count of associated PVCs not matched directly by the generated Schedule selectors |
| `pvcguardian_conflicting_units` | Gauge | Number of backup units skipped during the last reconciliation cycle due to conflicting configurations |

`/livez` always returns `200` while the process is running. `/readyz` (and the legacy `/healthz`) returns `503` on the leader until the first successful reconciliation and whenever the last success is older than three reconcile intervals (minimum 5 minutes). Standby replicas report ready so they can take over; they exit if they lose the election unexpectedly.

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
