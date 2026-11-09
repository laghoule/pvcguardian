# PVCGuardian Helm Chart

This Helm chart installs **PVCGuardian**, an automated Kubernetes daemon that discovers Helm releases in opt-in namespaces and maintains [Velero](https://velero.io) `Schedule` resources to back up their Persistent Volume Claims (PVCs) with spread execution windows.

---

## Prerequisites

- Kubernetes 1.26+ (aligned with supported versions for Velero v1.18+)
- Helm v3.8.0+ (required for `values.schema.json` validation support)
- [Velero](https://velero.io) v1.15+ deployed in the cluster with CRD `schedules.velero.io` installed

---

## Installation

### 1. Add the Chart / Local Installation

To install the chart directly from this repository:

```bash
helm install pvcguardian deploy/chart/pvcguardian \
  --namespace pvcguardian \
  --create-namespace \
  --values my-values.yaml
```

### 2. Verify Deployment

```bash
kubectl get pods -n pvcguardian -l app.kubernetes.io/name=pvcguardian
kubectl logs -n pvcguardian -l app.kubernetes.io/name=pvcguardian -f
```

---

## Configuration Reference

The following table lists the configurable parameters of the chart and their default values.

| Parameter | Description | Default |
|---|---|---|
| `replicaCount` | Number of PVCGuardian daemon replicas (runs as active/active or single instance) | `1` |
| `image.repository` | Image repository | `ghcr.io/laghoule/pvcguardian` |
| `image.tag` | Image tag (defaults to `Chart.appVersion`) | `""` |
| `image.pullPolicy` | Image pull policy | `IfNotPresent` |
| `imagePullSecrets` | Secrets for private image registries | `[]` |
| `serviceAccount.create` | Create dedicated ServiceAccount | `true` |
| `serviceAccount.name` | Custom ServiceAccount name | `""` |
| `podSecurityContext.runAsNonRoot` | Run container as non-root user | `true` |
| `podSecurityContext.runAsUser` | Non-root UID | `65532` |
| `securityContext.readOnlyRootFilesystem` | Mount root filesystem read-only | `true` |
| `securityContext.allowPrivilegeEscalation` | Prohibit privilege escalation | `false` |
| `resources` | CPU/Memory resource requests and limits | `{}` |
| `config.selectors.label` | Opt-in label key (expects `"true"`) | `pvcguardian.io/enabled` |
| `config.selectors.presetAnnotation` | Workload annotation key to choose preset | `pvcguardian.io/preset` |
| `config.selectors.requirePVC` | Only schedule releases having at least 1 PVC | `true` |
| `config.selectors.defaultPreset` | Fallback schedule preset name | `evening` |
| `config.selectors.excludeNamespaces` | Namespaces ignored by discovery scan | `["kube-system"]` |
| `config.selectors.namespaces` | Map of namespace name to default preset | `{}` |
| `config.velero.namespace` | Namespace where Velero and its Schedules reside | `velero` |
| `config.velero.storageLocation` | Velero BackupStorageLocation (empty = Velero default) | `""` |
| `config.velero.defaultVolumesToFsBackup` | Use Kopia filesystem backup for all volumes | `true` |
| `config.schedules.timezone` | Timezone for windows calculation | `UTC` |
| `config.schedules.retention` | Backup retention duration (`5d`, `120h`, `30m`) | `5d` |
| `config.schedules.windows.day.from` | Start of day window (HH:MM) | `10:00` |
| `config.schedules.windows.day.to` | End of day window (HH:MM) | `16:00` |
| `config.schedules.windows.evening.from` | Start of evening window (HH:MM) | `20:00` |
| `config.schedules.windows.evening.to` | End of evening window (HH:MM) | `23:00` |
| `config.schedules.windows.night.from` | Start of night window (HH:MM) | `01:00` |
| `config.schedules.windows.night.to` | End of night window (HH:MM) | `04:00` |
| `config.schedules.cron` | Map of named raw 5-field cron schedules | `{}` |
| `config.reconcile.interval` | Periodic reconciliation loop interval | `15m` |
| `config.logging.level` | Logging level (`debug`, `info`, `warn`, `error`) | `info` |
| `config.logging.format` | Logging format (`json`, `text`) | `json` |
| `config.metrics.enabled` | Enable Prometheus metrics endpoint | `true` |
| `config.metrics.addr` | Listening address for `/metrics` and `/healthz` | `:8080` |
| `podMonitor.enabled` | Create Prometheus Operator PodMonitor | `false` |
| `podMonitor.namespace` | PodMonitor namespace (default: release namespace) | `""` |
| `podMonitor.interval` | Scrape interval | `30s` |
| `podMonitor.scrapeTimeout` | Scrape timeout | `10s` |
| `prometheusRule.enabled` | Create Prometheus Operator alerting rules | `false` |
| `prometheusRule.namespace` | PrometheusRule namespace (default: release namespace) | `""` |
| `dashboard.enabled` | Deploy Grafana dashboard ConfigMap | `true` |
| `dashboard.namespace` | Target namespace for the dashboard ConfigMap | `""` (release namespace) |
| `dashboard.labels` | Labels for Grafana sidecar autodiscovery (`grafana_dashboard: "1"`) | `{ grafana_dashboard: "1" }` |

---

## Usage Example

### 1. Opt-in a Helm release for backup

Add the label on the release workload (Deployment, StatefulSet, DaemonSet, CronJob, Job):

```bash
kubectl label deployment my-app -n production pvcguardian.io/enabled="true"
```

### 2. Customize the schedule preset on the workload

To use another preset (e.g. `night` or `day` instead of `evening`):

```bash
kubectl annotate deployment my-app -n production pvcguardian.io/preset="night"
```

### 3. Check generated Velero Schedules

```bash
kubectl get schedules -n velero -l app.kubernetes.io/managed-by=pvcguardian
```

---

## AI-assisted development

This chart, its templates, its JSON Schema validation, and this documentation were created with the help of an AI coding agent (OpenCode), under human review and direction. Changes were validated with JSON Schema validation and Go unit tests before being committed.

---

## Uninstall

```bash
helm uninstall pvcguardian -n pvcguardian
```

*Note: Uninstalling PVCGuardian will stop the reconciliation loop, but existing Velero `Schedule` and `Backup` objects remain intact in the cluster.*
