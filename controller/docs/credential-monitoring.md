# Credential monitoring

NVKE observes actual certificate expiration dates independently of automated
renewal. Enable inspection on managed T2 nodes with the node agent that implements
`InspectKubernetesCertificates`. The controller polls joined nodes, persists
public observations in PostgreSQL and exports them without contacting nodes
while Prometheus scrapes `/metrics`. Inspection does not renew certificates,
write files, restart processes or request private key material over gRPC.

The current alpha inventory assumes the kubeadm layout NVKE provisions:

| Nodes | Public credential observations |
| --- | --- |
| Every joined node | kubelet client and serving certificates from their current PEM symlinks |
| Control-plane nodes | API server, API-to-kubelet client, front-proxy client, API-to-etcd client, etcd server, peer and healthcheck client |
| Control-plane nodes | Kubernetes, front-proxy and etcd CA certificates |
| Control-plane nodes | embedded client certificates in admin, controller-manager and scheduler kubeconfigs |
| Initial control-plane node, if present | super-admin kubeconfig client certificate |

`super-admin.conf` is optional: kubeadm init creates it, while joined control-plane
nodes need not have it. Its absence is visible without triggering the required
credential alert. A present optional certificate still receives expiry warnings.
The first inspection runs as soon as a node is marked joined, and a kubelet
serving certificate can still be missing then, while TLS bootstrap completes. A
required credential that is missing or invalid is therefore inspected again
after a backoff of 1, 2, 4, 8, 16 and then 32 minutes, capped at
`inspectionInterval`. With prompt probes, these retries fall approximately at
minutes 1, 3, 7, 15 and 31 after the first inspection.
`NVKECertificateMissingOrInvalid` needs the condition to hold for fifteen
minutes. The missing observation remains stored until a successful retry sees
the certificate, so slower bootstrap, delayed probes or timeouts can still
cause an alert. Early retries reduce that window; they do not guarantee that a
brief bootstrap gap cannot alert.

Custom certificate locations, external PKI, arbitrary kubeconfig certificate
references, worker bootstrap tokens and copied operator kubeconfigs outside this
inventory are not inspected. The current RPC reads only the fixed catalog and
caps each credential file at 1 MiB. It returns the earliest expiration and public
DER SHA-256 for a certificate bundle, never PEM, private keys, serials or subjects.
A fixed file that is missing or invalid remains unavailable. Monitoring CAs does
not enable CA rotation: approaching CA expiry requires an operator procedure
outside the automatic alpha renewal scope.

T1 management-cluster renewal remains an operator-run procedure.
This collector covers database-managed nodes; it does not discover unmanaged T1
nodes or scrape their credential files. Establish equivalent operator evidence
for those nodes rather than treating absent T1 series as healthy.

## Configuration

The existing renewal defaults remain a twelve-hour check interval and a thirty-day
renewal threshold. The following bounds are a **proposed alpha support policy**
for review, not a statement of product-approved SLOs. Startup checks the bounds
of each enabled feature and names the offending setting in the error. The
`certRenewal.*` bounds apply only while `certRenewal.controllerEnabled` is true,
and the `credentialMonitoring.*` bounds only while `credentialMonitoring.enabled`
is true. The credential metrics collector is registered either way and reads
`credentialMonitoring.observationMaxAge` and `credentialMonitoring.warningDays`,
so those two must be positive even while monitoring is disabled. The chart
refuses the same values when it renders, so `helm install` or `helm upgrade`
fails with the offending setting instead of the new pod failing to start.

These settings live in the `nke-controller-config` file (the chart renders them
from `scheduledMaintenance.certRenewal.*` and `credentialMonitoring.*`). The
controller no longer reads them from environment variables, and the chart
refuses an `env` entry that names one.

| `nke-controller-config` key | Chart value | Default | Supported bound or relationship |
| --- | --- | --- | --- |
| `certRenewal.interval` | `scheduledMaintenance.certRenewal.interval` | `12h` | `1h` through `24h` |
| `certRenewal.thresholdDays` | `scheduledMaintenance.certRenewal.thresholdDays` | `30` | `7` through `90` days |
| `credentialMonitoring.enabled` | `credentialMonitoring.enabled` | `true` | controls read-only node and CSR observations independently of renewal |
| `credentialMonitoring.inspectionInterval` | `credentialMonitoring.inspectionInterval` | `1h` | `5m` through `12h` |
| `credentialMonitoring.observationMaxAge` | `credentialMonitoring.observationMaxAge` | `3h` | at least two inspection intervals; at most `24h` |
| `credentialMonitoring.warningDays` | `credentialMonitoring.warningDays` | `45` | `9` through `120` days; exceed the renewal threshold by at least two renewal intervals |

The warning margin gives an operator notice before the automated renewal
threshold, with at least two check opportunities. A node-agent connection and
inspection attempt has a thirty-second timeout. Unreachable or older agents
retain last-known expiry dates but set inspection availability to zero. A failed
or incomplete inspection is retried after a short backoff (1 minute, doubling to
32 minutes, never beyond `inspectionInterval`) rather than after a full interval,
so a transient agent restart or network error normally clears before
`NVKECertificateInspectionUnavailable` has held for fifteen minutes. A successful
inspection with every required credential present, or an agent that does not
implement the RPC, waits the full interval. No prior
observation yields an age of `-1`. Expiration alerts use only fresh observations;
unavailable inspection alerts cover unknown or stale data. Re-enabling monitoring
resumes persisted observations and refreshes them on the configured interval.

The tenant manager reads a complete kubelet-serving CSR snapshot every minute.
It stores the snapshot when the counts or the oldest pending time change, and
otherwise refreshes the stored observation time only every two to three minutes.
The write goes around the cluster reconcile notifications, so observing does not
queue a reconcile in every cluster-watching controller. Approved requests without
an issued certificate still count as pending. Failed and denied requests remain
visible until the Kubernetes CSR object is removed; counts are inventory gauges,
not lifetime event counters. A missing or stale
snapshot has its own alert, so zero cached counts cannot imply health when tenant
access has failed.

## Metrics and labels

Every metric has the `nvke_controller_` prefix. Identity labels are limited to
managed `tenant_cluster` and `node`, plus the fixed `credential` catalog or fixed
lifecycle `state` values. There are no CSR names, Secret names, workflow/request
IDs, fingerprints, certificate subjects or arbitrary error text in labels.
Fingerprints are stored as public evidence, never labels. Deleting a node or
cluster removes its series on the next scrape; a controller restart reconstructs
metrics from database state.

| Metric suffix | Meaning |
| --- | --- |
| `certificate_not_after_timestamp_seconds` | actual X.509 NotAfter, retained for diagnosis when inspection becomes unavailable |
| `certificate_required` | required credential applicability; zero for optional super-admin |
| `certificate_observation_available` | credential found by the last successful inspection, while that inspection is within `observationMaxAge`; a later failed attempt does not clear it |
| `certificate_inspection_available` | fresh, successful latest node inspection; zero after any failed attempt |
| `certificate_observation_age_seconds` | seconds since the last successful inventory response; `-1` for never observed |
| `certificate_warning_window_seconds` | configured expiry warning window |
| `credential_monitoring_enabled` / `renewal_automation_enabled` | independent subsystem enablement |
| `renewal_overdue_seconds` | seconds past the next scheduled renewal check, zero before due; until the controller schedules the first check, it is taken as cluster creation plus `certRenewal.interval` |
| `renewal_state` | one-hot unknown, accepted, succeeded or failed latest cycle |
| `renewal_active_seconds` | age of an accepted cycle |
| `renewal_mutation_lock_retained` | failed renewal still retains exclusive mutation ownership |
| `serving_csr_count` | snapshot pending, failed or denied counts |
| `serving_csr_oldest_pending_age_seconds` | age of oldest unissued serving request |
| `serving_csr_observation_age_seconds` | CSR snapshot freshness; `-1` for never observed |
| `etcd_key_rotation_state` | one-hot unknown, running, succeeded or failed latest rotation |
| `etcd_key_rotation_progress_age_seconds` | running rotation age since the last phase progress |
| `credential_metrics_collection_success` | database inventory queries succeeded |

A renewal due timestamp is a scheduling timestamp. It is never substituted for
certificate expiry. Unknown rotation state means no recorded rotation and is
not itself a failed operation.

## Install and evaluate alerts

The controller chart can install the shipped `PrometheusRule`:

```yaml
serviceMonitor:
  enabled: true
  labels:
    release: prometheus
credentialMonitoring:
  enabled: true
  inspectionInterval: "1h"
  observationMaxAge: "3h"
  warningDays: 45
  alerts:
    enabled: true
    labels:
      release: prometheus
```

`release: prometheus` is an example selector label. Use labels matching your
Prometheus instance's `serviceMonitorSelector` and `ruleSelector`. Install
Prometheus Operator CRDs before enabling the optional rule. The umbrella
`global.monitoring.enabled=false` disables both monitoring resources. The chart
does not install Prometheus, a rule evaluator or an Alertmanager notification
route. Successful scraping alone does not deliver notifications.

Operators must confirm the ServiceMonitor selects the controller service, the
Prometheus instance loads `nvke-credential-lifecycle`, and its Alertmanager route
has a receiver for the rule severities. Add a separate scrape/target-down alert:
these rules cannot report from a controller that is unreachable. When intentionally
disabling inspection, disable or silence its availability rules explicitly;
unknown observations must not be mistaken for healthy credentials.

## Alert responses

The shipped thresholds below are proposed alpha operational bounds. Expiry uses
the configurable warning window; the remaining rules use fixed evaluation
thresholds in `deploy/charts/controller/files/credential-alerts.yaml`.

| Alert | Threshold and hold | Operator response |
| --- | --- | --- |
| `NVKECertificateExpiring` | NotAfter minus current time below warning window for `5m` | inspect node inventory and renewal status; use manual CA procedure for a CA |
| `NVKECertificateInspectionUnavailable` | failed, never observed or stale node inspection for `15m` | restore transport, upgrade unsupported agent, confirm layout and observation freshness |
| `NVKECertificateMissingOrInvalid` | required credential unavailable in a successful fresh inventory for `15m` | inspect fixed file or kubelet bootstrap; validate identity before repair |
| `NVKECertificateRenewalFailed` | latest cycle failed for `5m` | follow [renewal recovery](certificate-renewal.md); released failures retry after one hour, retained-lock failures stay halted |
| `NVKECertificateRenewalOverdue` | more than one hour overdue for `15m`, when automation enabled | inspect readiness, Temporal and maintenance ownership |
| `NVKECertificateRenewalStuck` | accepted cycle older than two hours for `15m` | inspect workflow and agent termination/restoration before retry |
| `NVKEKubeletServingCSRStale` | unissued CSR older than fifteen minutes for `5m` | inspect approver decisions and signer health; approved is not issued |
| `NVKEKubeletServingCSRFailed` | failed or denied CSR exists for `5m` | inspect tenant conditions and identity; never blindly approve a denied request |
| `NVKEKubeletServingCSRObservationUnavailable` | never observed or snapshot older than five minutes for `5m` | restore tenant-manager access and kubeconfig refresh |
| `NVKEEtcdKeyRotationFailed` | latest rotation failed for `5m` | follow [phase-specific recovery](etcd-encryption-key-rotation.md#phase-aware-recovery): a released `prepare` failure needs a new workflow ID; later phases retain the original owner |
| `NVKEEtcdKeyRotationStuck` | no phase progress for two hours for `15m` | inspect workflow/rewrite before reset or key removal; large rewrites may need longer investigation |
| `NVKECredentialMetricsCollectionFailed` | inventory query failure for `5m` | restore controller database connectivity; absent cluster series do not prove health |

Alert summaries and responses are shipped in each rule. Renewal or rotation
failure does not establish that the agent stopped mutating state. Do not release
locks or start competing maintenance from an alert alone.

## Offline verification and live acceptance

`make chart-check`, which the merge gate runs, includes the credential rule
fixtures, a schema-validated render with alerts enabled, and renders asserting
that no PrometheusRule is emitted by default or with
`global.monitoring.enabled=false`. The check installs
Prometheus `promtool` at the version in `make/tool-versions.mk` into the ignored
`.local/` directory, verifying the release archive against a committed SHA-256.
The first run needs network access to GitHub releases; subsequent runs reuse the
binary. To run only the credential checks:

```bash
make -C controller credential-alerts-check
make -C controller/deploy/charts test
```

Put the repository-pinned Helm and kubeconform on PATH for these checks. Fixtures
exercise warning crossing before expiry, holds, firing and recovery for every
shipped alert; optional super-admin absence and unavailable observations are
also covered. Fixture format follows the [Prometheus rule-testing reference](https://prometheus.io/docs/prometheus/latest/configuration/unit_testing_rules/).

Offline tests do not prove a site's scrape selection, rule evaluation, actual
certificate layout or notification delivery. Before acceptance, retain sanitized
live evidence of a supported node approaching the warning window, the loaded
rule firing before expiry, receipt by the configured receiver, and resolution
after renewal. Exercise unavailable inspection and a stale/failed CSR as well.
Offline results do not establish live renewal qualification.
