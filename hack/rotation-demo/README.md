# Credential rotation qualification demo

`demo.sh` runs real qualification checks against one explicitly selected cluster.
It uses `kubectl`, Temporal CLI, the NVKE controller REST API, read-only PostgreSQL and
Prometheus. It writes only public, allowlisted JSON evidence. No live run is
included in this repository. Offline tests exercise collector failures and
redaction; they do not qualify a deployment.

The supported baseline is kubeadm static pods with stacked etcd and three
control-plane nodes behind the selected VIP (`kubeadm-ha`). A single control
plane (`kubeadm-single`) has expected API interruption during renewal; declare
an explicit error/latency budget for that profile. External etcd, external PKI,
custom certificate layouts and CA rotation require other procedures.

For `etcd-key`, follow the [rotation prerequisites](../../controller/docs/etcd-encryption-key-rotation.md#prerequisites):
Kubernetes **1.34+**, agents from the **same qualified build as the controller**,
and node-local loaded-config/process-start metrics are required; counter-only
proof is unsupported. Quiesce independent filesystem/configuration writers as
well as other controller mutations. Offline demo tests do not qualify these
prerequisites or phase-aware failure/reset recovery.

## Prepare the environment

Use Python 3.10+, a Kubernetes-compatible `kubectl`, Temporal CLI 1.1.1 or later
(the lanes use JSON [workflow describe](https://docs.temporal.io/cli/command-reference/workflow)
and `workflow start --id-conflict-policy Fail`), `psql` and OpenSSL. T1 collection also needs SSH,
noninteractive privileged access, GNU `date`, `sha256sum`, `openssl`, `base64`
and `kubectl` on the control-plane nodes. The script never prints command diagnostics or histories.
Temporal connection/TLS credentials use its existing CLI environment/profile;
PostgreSQL uses `PGSERVICE` and `PGPASSFILE` with a read-only role. Keep those
files and kubeconfigs outside Git.

Copy [config.example.json](config.example.json) to an operator-owned directory.
Replace every example value with the approved site's values. `KUBECONFIG`
selects the kubeconfig; `context` must select the exact tenant and `api_server`
must equal that context's HTTPS server (the VIP for HA traffic). Record the actual
`kube-system` namespace UID, the controller's cluster UUID/name and complete joined node
UUID/name/role inventory. Continuous API traffic is pinned to `api_server` and
must keep observing that `kube-system` UID; any target drift fails the run with
`traffic_target_mismatch`, independent of the API outage budget. The collector rejects mismatches, added/missing nodes,
unsupported control-plane counts and unhealthy endpoints before starting work.

Set `deployment_source_revision` to the clean checkout HEAD used to build the
deployed candidate. Set `expected_versions.controller` to `/api/version.version`,
`agents` to the expected deployed agent version for every node UUID, and
`kubernetes` to each node's expected `kubeletVersion`. The collector compares
reported versions before/after and stores their SHA-256 hashes, never arbitrary
version strings. The agent value comes from the controller's persisted registration;
refresh/verify registration and retain image/binary digests separately. Version
reporting and the operator-declared source mapping do not cryptographically
prove a binary's source. Do not qualify an unrelated deployment by copying
unexpected reported versions into the expected map.

For authenticated HTTP services, optional `cwe_authorization_file` and
`prometheus_authorization_file` name private files containing the complete
Authorization header value. The collector rereads each file on every request.
With controller authentication enforcing, `cwe_authorization_file` must hold
`Bearer <token>`, with a token minted for the controller's audience (see
[Authenticating to the API](../../controller/docs/authenticating-to-the-api.md)).
`controller/hack/nvke-token.sh` prints the bare token, so add the `Bearer `
prefix yourself. The token must stay valid until the lane ends: either mint it
with `NKE_TOKEN_TTL` covering `timeout_seconds`, or rewrite the file in place
before it expires. Once the lane has started, an invalid or expired credential
is reported only as `service_unavailable`, and a renewal lane that has mutated
cannot be repeated on that cluster. URLs/paths/headers never enter evidence. `cwe_url`
and `prometheus_url` must use `https://`, and the collector refuses HTTP
redirects so the header cannot leave for another host or scheme. TLS uses
normal system trust; no insecure bypass is provided. `kubelet_ca_file` is the
public Kubernetes CA used to authenticate each configured `kubelet_address:10250`.
Permit direct access to all API servers and kubelet serving endpoints.

Deploy the inspection, monitoring and audit changes first. Apply migrations,
enable the appropriate ServiceMonitor/PrometheusRule selection and establish
Alertmanager routing. All nodes need fresh complete public observations. The
installed kubelet-serving certificate of each managed T2 node must be the one
the kubelet actually serves on its configured address, authenticated with
`kubelet_ca_file`. A matching approved and issued CSR is recorded as optional
evidence only while it is still listed: kube-controller-manager deletes approved
and issued CSRs after about an hour, and none of these workflows creates serving
CSRs. A certificate that is not served, unknown certificate, stale inspection,
missing series or missing audit action fails the run. The collector does not
repair these states or approve CSRs. See
[CSR troubleshooting](../../docs/tenant-clusters/kubelet-serving-certificates.md)
and [monitoring](../../controller/docs/credential-monitoring.md).

Reserve a change window, backups and supported recovery access. Do not overlap
renewal, key rotation, upgrades or recycling. Do not restart or redeploy the
controller while a lane runs. Select a timeout long enough for
the next inspection (default one hour), scheduler check and etcd rewrite (up to
six hours). `timeout_seconds` allows up to eight hours; timing out observation
does not terminate/reset a workflow or release its mutation lock.

## Run the T2 scenarios

Run the T2 lanes in this order on one cluster: `scheduled-renewal` first, then
`alert-renewal` and `forced-renewal`, with `etcd-key` independent of them. Forced
and alert renewal both force-renew every control plane, so either one run first
makes the scheduled cycle `unchanged` (which fails that lane) and clears the
warning window the alert lane needs; `aggregate` requires all T2 reports from
the same cluster, which then cannot qualify until its certificates approach
expiry again. The alert lane also needs staggered expiry prepared in advance:
one still-valid control-plane leaf on `alert_node` already inside the warning
window and its `NVKECertificateExpiring` alert firing. Every leaf on
`alert_node` must stay inside `warningDays` but outside
`certRenewal.thresholdDays` (30–45 days left with the defaults) until the alert
lane starts; otherwise the scheduled lane or the background scheduler renews it,
the alert resolves and `alert-renewal` fails with `expiry_alert_not_firing`. Use
a different control-plane node, with a leaf inside `thresholdDays`, as the
scheduled lane's candidate. See
[Prove expiry alert delivery and resolution](#prove-expiry-alert-delivery-and-resolution).

Run from the clean candidate checkout. Output paths must be new and outside the
repository, so subsequent runs remain clean. The exact cluster UUID supplied to
`--approve-cluster` is the operator's opt-in. Omitting it performs baseline
checks and fails before creating fixtures or starting workflows. The first lane
is `scheduled-renewal`; start it at the time described below:

```bash
hack/rotation-demo/demo.sh run --config /operator/private/t2.json \
  --scenario scheduled-renewal \
  --approve-cluster 00000000-0000-0000-0000-000000000001 \
  --output /operator/private/scheduled-renewal.json
```

Then run the same command with `--scenario alert-renewal` and, last,
`--scenario forced-renewal`, each with a new output path.

`forced-renewal` starts `RenewK8sCertsV2` with a fresh operator workflow ID,
positive threshold and `force:true`. It requires a completed Temporal execution
and matching succeeded cluster owner, cleared mutation/failure state, changed
leaf fingerprints with later expiry on every control plane, unchanged CA
fingerprints, replaced static-pod containers, fixed etcd membership with every
endpoint healthy, all nodes Ready, fresh expiry/CSR telemetry, installed and
served kubelet certificate matches, and per-node renewal/verification audit
events. Public fingerprints and expiry come from persisted read-only inspection,
including a fresh inspection after terminal workflow completion.

Baseline and final telemetry checks wait within the original `timeout_seconds`
deadline for expiry series that match the independent public certificate
snapshot. The final check requires source samples and successful certificate/CSR
observations after terminal completion. Values and `timestamp(...)` source
sample times come from one PromQL evaluation; an instant query's response time
alone does not establish a new scrape. The collector adds elapsed time since the
scrape to sampled age gauges, keeping the CSR observation limit at 300 seconds.
Every returned controller replica must meet these checks. Keep operator,
controller and Prometheus clocks synchronized for these wall-clock comparisons.

`scheduled-renewal` observes the actual controller scheduler. Start it only when the cluster's
`nextCertRenewalDue`, plus the renewal itself and the next certificate
inspection (default one hour), fits inside `timeout_seconds`, which is at most
eight hours while `certRenewal.interval` defaults to twelve. Read
`nextCertRenewalDue` from `GET /api/clusters/{clusterID}` first. Also start it
early enough that the baseline checks, including the fresh-metrics wait, finish
before `nextCertRenewalDue`: a cycle the scheduler accepts before the lane
starts its traffic fails the lane with `cycle_started_before_traffic`, and that
cycle has already renewed the due node. It waits for a new non-forced,
scheduler-named cycle; it does not synthesize a schedule by starting Temporal
directly. Prepare a healthy, naturally approaching-expiry candidate within the
configured selection threshold. The cluster must already have a
`nextCertRenewalDue`: the controller sets it one renewal interval after it first
sees the cluster and schedules nothing before then. An ordinary scheduled no-op cannot prove
renewal and fails this scenario. The control planes may have staggered expiry:
the scheduled lane reads each node's cycle-scoped renewal audit outcome and
requires changed leaf fingerprints, later expiry and replaced static-pod
containers on renewed nodes. Nodes with `renew/no_op` and reason `unchanged`
must preserve their healthy leaves and containers and have successful postflight
verification audit coverage. Missing or conflicting per-node renewal outcomes
fail; at least one node must independently prove renewal. The final audited
attempt of each executor activity determines its outcome: an earlier failed
attempt followed by a successful retry is valid and remains visible in the
audit evidence. Attempt numbers do not order different activities or workflow
runs, whose terminal outcomes must agree. All CA fingerprints,
etcd membership, target, traffic and telemetry checks still apply to every node.
The added per-node evidence records only node UUIDs and the fixed
`renewed`/`unchanged` outcomes. Workflow histories and runbook output stay private.
Run this lane before forced
renewal, which moves the next due time and resets leaf expiry. The naming
convention is correlation, not authenticated proof of the human who started a
workflow.

`etcd-key` starts `RotateEtcdEncryptionKey` only for an already encrypted cluster.
It creates one public marker Secret before rotation, checks encrypted secretbox
storage, continuously reads its unchanged payload through every API server,
then requires a changed storage-header SHA-256 and matching newly written probes
through every API server after completion. Raw storage values remain private in
memory and protected command capture, and never enter evidence. It also requires
unchanged certificate/container identities and all rotation phase/check and
terminal rotate audit observations. Successful runs remove their public fixtures
after observation stops; failed runs may leave fixtures for diagnosis. Follow
[phase-aware recovery](../../controller/docs/etcd-encryption-key-rotation.md#phase-aware-recovery)
before removing anything or starting another operation.

Every scenario continuously reads the API during work and checks the declared
error fraction and maximum observed latency. Key rotation additionally reads
the preexisting Secret from every API server each iteration. These are bounded
serial read probes, not a load benchmark or a guarantee against a shorter
unsampled interruption. Any Secret-read or payload failure is fatal regardless
of the API outage budget. The observer waits for its bounded in-flight command
batch before finishing; it does not delete the preexisting marker on failure.
Evidence includes sample/error counts and maximum request-batch latency; health
snapshots independently check actual etcd members,
non-learner state and all cluster endpoints from every member.

## Prove expiry alert delivery and resolution

`alert-renewal` is a separate forced-renewal lane. Prepare a still-valid leaf in
the warning window, keeping every leaf on `alert_node` outside
`certRenewal.thresholdDays` as described in
[Run the T2 scenarios](#run-the-t2-scenarios), wait for `NVKECertificateExpiring` to fire, and establish a
real receiver before running it. Never shorten/replace certificates on a live
production site just to manufacture this test. Set `alert_node`,
`alert_credential`, `alert_receipts_file` and `alert_receipts_since` (within the
last day and before run start). CA alerts cannot use this lane because renewal
does not rotate CAs.

The bundled receiver is **required**: the collector accepts only its
`nke.alert-receipt.v1` records, not a site receiver's. Start it and route
Alertmanager to it before the alert fires, so that a matching firing receipt is
already in `alert_receipts_file` when the lane starts; the collector refuses to
start the renewal (`firing_notification_not_received`) otherwise. It accepts
actual Alertmanager webhook requests at `/alerts`:

```bash
python3 hack/rotation-demo/alert-receiver.py \
  --bind 127.0.0.1 --port 9087 --output /operator/private/alert-receipts.jsonl
```

Route the evaluator's Alertmanager to this operator-controlled address via the
site's approved reachable forwarding/proxy. Keep the listener behind existing
access controls and enable `send_resolved: true`. The default loopback bind
requires approved forwarding; a remote Alertmanager cannot reach it directly.
This helper stores only fixed alert status, receipt ID, receipt time, credential
name and hashes of target labels. It drops annotations, arbitrary labels, URLs
and message bodies. Protect the receiver and receipt file against fabricated
requests; it authenticates no sender itself. A receipt proves observation at
this configured webhook, not delivery to an unrelated email/pager receiver.

The scenario requires loaded healthy rules, firing before actual expiration,
renewed public credentials, resolved evaluator state, and both firing/resolved
webhook receipts with the same alert ID matching the target/window. The firing
receipt must precede mutation and the resolved receipt must follow the start of
this run's renewal, with no new firing receipt between that resolution and
completion; an old resolution cannot satisfy the check. Renewal proceeds one
node at a time, so the alert can resolve before the last node finishes. Missing delivery or resolution
fails. Alert fixture tests and absent series cannot substitute for these checks.

## Observe manual T1 separately

Use [the separate T1 runbook](../../docs/site-operations/certificate-renewal.md),
with the
[additional evidence guidance](../../docs/operations/credential-lifecycle.md#manual-t1-evidence-and-qualification).
The demo does not duplicate that procedure or automate its mutations. Prepare a
separate T1 configuration with `cluster_id` (also the `--approve-cluster` value
and part of the completion marker), `api_server`, `profile`, the site
`context`/`kube_system_uid`, stable UUID evidence identities for nodes, complete
node inventory, expected Kubernetes versions, SSH destinations for control
planes, unique `manual_operation`, a nonexistent `manual_completion_file`,
`manual_audit_file`, and `timeout_seconds`. Size `timeout_seconds` for the whole
T1 runbook and its post-checks, at most 28800 (eight hours): the same deadline
bounds the wait for the completion marker, and the 7200-second default fails a
longer maintenance with `manual_completion_not_observed` after it has happened. Set `deployment_source_revision` to this demo revision;
manual T1 has no controller/agent registration requirement. T1 inventory intentionally
covers kubeadm leaves/configs and CAs, and excludes kubelet certificates/CSRs.

Start `--scenario manual-t1` with that config and explicit approval. Wait for
the fixed message **"T1 baseline recorded and API observation running"** before
beginning the runbook. The read-only SSH collector runs
[public-certificates.sh](public-certificates.sh) under `sudo`: fixed OpenSSL
public-certificate queries, and for each kubeadm kubeconfig a
`kubectl config view --raw` read whose embedded client certificate is decoded
with `base64` to compute its public fingerprint and expiry. It never copies PKI
or kubeconfigs off the node. T1 uses actual
public snapshots rather than the T2 database inventory.

Record check/renew/verify observations for each control-plane node through the
existing [manual audit tool](../../controller/docs/credential-audit.md), using
the exact `kube-system` UID as `--cluster`, node name as `--node`, and the
configured `manual_operation` as `--operation`, with a per-step suffix
`/<action>-<n>` (for example `example-operator-change-window/renew-2`), where
`<action>` is `check`, `approve`, `renew`, `rotate` or `verify` and `<n>` is 1
through 99 without leading zeros, whenever
a step repeats with otherwise identical flags. The collector rejects other
operation values with `manual_audit_target_or_run_mismatch` and duplicate
`event_id`s, which a repeated step without its own suffix produces, with
`audit_event_duplicate`. Keep the original audit records: do not delete rows or
repeat maintenance to manufacture evidence.
Before starting `manual-t1`, build the tool once from the repository root, as in
the [audit guide](../../controller/docs/credential-audit.md#manual-t1-evidence):

```bash
tool_dir="$(mktemp -d)" &&
  (cd controller && go build -o "$tool_dir/credential-audit" ./cmd/credential-audit)
```

Then record each observation only after that step was performed on that node.
For example, the `verify` observation for `example-cp-1`, after its `check` and
`renew` rows were recorded with the same tool:

```bash
"$tool_dir/credential-audit" --action verify --credential kubeadm_leaf_bundle --plane t1 \
  --cluster 00000000-0000-0000-0000-000000000002 --node example-cp-1 \
  --operation example-operator-change-window/verify-1 --actor example-operator \
  --outcome success --reason verified >> /operator/private/t1-audit.jsonl
```

Keep `$tool_dir` for the later observations in the same shell.

Only after every runbook step and refreshed operator kubeconfig pass, create
the completion marker (use the configured identities):

```bash
cat > /operator/private/t1-completed.json <<'EOF'
{"cluster_id":"00000000-0000-0000-0000-000000000001","kube_system_uid":"00000000-0000-0000-0000-000000000002","completed":true}
EOF
```

The collector then independently verifies later expiry/changed leaves,
unchanged CAs, replacement of all four static-pod components, node readiness,
unchanged healthy etcd membership, API traffic and recent audit events bound
to this exact target/run. T1 audit actor attribution remains `operator_reported`;
neither the marker nor JSONL claims authenticated automatic execution. The
report records the T1 exclusions explicitly.

## Collect release evidence

Each run exits nonzero on any required failure, unknown input, timeout or output
failure. The output destination is exclusively reserved with mode `0600` before
collection or mutation; existing/unwritable output paths are rejected early.
It includes source/deployment revisions, dirty-source flag,
profile/topology/target identity, timestamps, checked stages, public before/after
expiry/fingerprints, hashed deployed versions and workflow correlation, traffic
summary, allowlisted audit references and notification receipts where required.
It contains no keys, certificate PEM, kubeconfig, CSR body, Secret manifest,
command output or workflow history. Keep original credentials/backups elsewhere.

After all five scenarios, create a release index:

```bash
hack/rotation-demo/demo.sh aggregate --output /operator/private/release.json \
  /operator/private/forced-renewal.json /operator/private/scheduled-renewal.json \
  /operator/private/alert-renewal.json /operator/private/etcd-key.json \
  /operator/private/manual-t1.json
```

The index records evidence digests and missing/failed scenarios. It exits
nonzero with `incomplete` if any required scenario is absent, failed, synthetic,
or collected from a dirty checkout. Reports without a Boolean `source_dirty`, or
passed reports without well-formed hashed deployed versions, are rejected. All T2 reports must identify the same source,
cluster UID/UUID and profile, and the same observed deployed versions (passed
reports); a redeploy between lanes fails with `release_deployed_versions_mismatch`.
A passed index covers these specified scenarios;
it does not claim CA rotation, failure/reset recovery, unsupported topologies,
audit delivery completeness, or an unrelated notification receiver. Retain
separate reviewed evidence for those acceptance criteria. Audit ledger writes
are best effort after state changes; missing observations fail collection and
must never be manufactured to fill a gap. The audit-write failure counter is
controller-wide: another tenant's failed or rate-limited write fails this lane
with `audit_writes_failed_during_lane`, even if this lane's mutation succeeded.
The failure series must match the baseline exactly, so a controller rollout or
replica change during the lane, which changes the series, always ends it with
`audit_writes_failed_during_lane`; an in-place controller restart ends it the
same way when it resets a nonzero failure counter.

## Offline checks

```bash
make -C hack/rotation-demo test lint
make docs-check
```

The stdlib test suite uses in-process fakes and local processes only. It checks
completion/ownership gates, target and metrics failures, timeouts, approval,
public fingerprint/expiry and unchanged-CA comparisons, continuous traffic
budgets, old-Secret rewrite/primary convergence, missing notifications/scenarios,
private/exclusive output and command/webhook redaction. These tests provide no
live release evidence.
Reports from the fake collector carry `live: false` and cannot pass
aggregation.
