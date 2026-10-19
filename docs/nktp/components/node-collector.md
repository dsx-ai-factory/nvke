# Node collector

The node Collector (`daemonset`) runs on every node. It collects the node's logs
and metrics and forwards OTLP that workloads send to it.

## OTLP endpoint

The node Collector accepts OTLP logs, metrics, and traces at
`nktp-daemonset-collector:4317` over gRPC and `http://nktp-daemonset-collector:4318`
over HTTP. The Service routes traffic only to the Collector on the sender's node.
If no local Collector is ready, traffic fails rather than reaching another node.
The Collector sets `k8s.node.name` and forwards each signal to its corresponding
gateway. Qualify the Service name with the NVKE telemetry pipelines (NKTP) namespace when
sending from another namespace.

## Pod logs and metrics

The DaemonSet reads pod logs from `/var/log/pods` and sends them to the logs
gateway. A dedicated Target Allocator assigns monitored metric endpoints only
to collectors on the same node. Metrics go to
`nktp-gateway-metrics-collector:4317` over OTLP/gRPC, which requires the metrics gateway.
Per-process host metrics are not collected by default. Node exporter provides
host metrics, and the kubelet cAdvisor scrape provides per-container resource
use. See [Enable host process metrics](../guides/enable-host-process-metrics.md)
to enable the `hostmetrics` process scraper.

Each DaemonSet collector also scrapes the kubelet `/metrics` and
`/metrics/cadvisor` endpoints on its own node. See
[Configure kubelet metrics](../guides/configure-kubelet-metrics.md) to change these scrapes, verify
kubelet certificates, or run NKTP alongside kube-prometheus-stack.

## Journal

The DaemonSet also collects systemd journal entries at info priority and above
and forwards them to the logs gateway. Nodes must provide `/usr/bin/journalctl`
and `/etc/machine-id`, with journals in `/var/log/journal`. The Collector uses
selective read-only host mounts and
the `SYS_CHROOT` capability to run the host executable. The `/lib64` host volume
is also mounted at `/hostfs/usr/lib64` for Red Hat library paths. Collection
starts with new entries. Journal cursors are not persisted across Collector restarts.

Override `root_path`, `journalctl_path`, and `directory` under
`daemonset.otelcol.config.receivers.journald` for a different host layout.
Update `daemonset.kubernetes.volumes` and `volumeMounts` to match, retaining
the other mounts when replacing either list. For runtime journals, add a
`/run/log/journal` host volume mounted at `/hostfs/run/log/journal` and set the
receiver directory to `/run/log/journal`.

See [Log classification](#log-classification) for the default audit and
control plane log sources.

## Log classification

NKTP classifies logs by source using the `nvidia.telemetry.class` resource
attribute. All records from a classified source receive the same class,
regardless of their content. The class is forwarded to the logs gateway for
use in routing rules.

| Source | Classification | `service.name` | Recognition |
| --- | --- | --- | --- |
| sudo and SSH journal entries | `system.auth` | `journald` | Journal producer identifier is `sudo` or `sshd`. |
| Linux authentication files | `system.auth` | `auth` | All records from `/var/log/auth.log` or `/var/log/secure`. |
| Linux auditd | `system.audit` | `auditd` | All records from `/var/log/audit/audit.log`, including authentication records. |
| Kubernetes control plane logs | `k8s.control_plane` | Not set by NKTP | Pods in `kube-system` with matching pod-name prefixes and container names for `kube-apiserver`, `kube-controller-manager`, `kube-scheduler`, or `etcd`. |
| Kubernetes API audit logs | `k8s.api.audit` | `kube-apiserver` | Records from `/var/log/kubernetes/audit/audit.log`. API audit logging must be enabled on the API server. |

Other locally collected logs remain unclassified. All journal records use
`journald` as `service.name`. Shared authentication files use `auth`. Service
names reach the logs gateway alongside the classification.

## Self-telemetry

The collector's own metrics, logs and traces go through its
[sidecar](../architecture/self-telemetry.md) to watchdog.
Target Allocator telemetry is sent to watchdog.
Each collector also reads Target Allocator stdout and stderr logs on its node
in the release namespace and sends them to watchdog through `logs/targetallocator`.
Operator manager container logs go to watchdog through `logs/operator`.

## Configuration

Use `daemonset.otelcol.config` for Collector configuration and
`daemonset.kubernetes` for Kubernetes workload configuration. The pod-log mount
is read-only, but host file permissions require the collector to run as root.
The file reader follows new records and does not persist offsets across restarts.
Retain `GOMEMLIMIT` and `GOGC` when replacing its environment list.
The chart adds `NODE_NAME`, `POD_NAMESPACE`, and `OTEL_K8S_NODE_IP` if the list
omits them.

## Target Allocator

Setting `daemonset.kubernetes.targetAllocator.enabled` to `false` will cause Helm
rendering to fail.

Monitoring CRs labeled `nktp.nvidia.io/large-target: "true"` are excluded.
Unlabeled monitors remain eligible. Targets without a matching collector node
remain unassigned.

The Target Allocator rejects ServiceMonitor and PodMonitor endpoints that reference
local bearer-token or TLS files in the Collector container.

### Monitor credentials

To grant the allocator access to Secrets and CA ConfigMaps referenced by monitors, list
the namespaces containing those monitors and their referenced objects:

```yaml
daemonset:
  kubernetes:
    targetAllocator:
      prometheusCR:
        secretNamespaces: [metrics-monitors]
```

`daemonset.rbac.create` defaults to `true`. For each explicitly
listed namespace, Helm creates a Role granting Secrets `get/list/watch` and
ConfigMaps `get`, bound to the allocator ServiceAccount in the release namespace.
The binding honors `daemonset.kubernetes.targetAllocator.serviceAccount`.
Omitting the list or setting it to `[]` creates no namespace RBAC. Set
`daemonset.rbac.create: false` to manage these grants externally.

Grants cover all Secrets in each listed namespace. Use namespaces with trusted
monitor authors. Secret and CA references resolve in the monitor's namespace.
The namespaces must already exist. `secretNamespaces` controls Secret watching.
Configure monitor and namespace selectors separately.
