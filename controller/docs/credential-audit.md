[//]: # "SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved."
[//]: # "SPDX-License-Identifier: Apache-2.0"

# Credential lifecycle audit evidence

NVKE records OSS-alpha T2 credential lifecycle observations in the controller's
PostgreSQL `credential_audit_events` table. Operators record manual T1 procedures
with the same versioned schema using `credential-audit`. Neither path contains
private keys, encryption keys, kubeconfigs, certificate PEM, command output,
Secret names, or raw upstream error messages.

## Schema: `nke.credential-audit.v1`

| Field | Contract |
| --- | --- |
| `schema_version` | `nke.credential-audit.v1` |
| `event_id` | SHA-256 semantic observation ID; timestamp excluded |
| `timestamp` | Observation time in UTC, not certificate validity or mutation start time |
| `action` | `check`, `approve`, `renew`, `rotate`, `verify` |
| `credential_type` | `kubeadm_leaf_bundle`, `kubelet_serving`, `etcd_encryption_key` |
| `plane` | `t1` or `t2` |
| `target` | Managed `cluster_id`/optional `node_id` UUIDs, or external T1 `cluster_hash`/optional `node_hash` |
| `actor` | `kind`: `automatic`, `operator_unattributed`, or `operator_reported`; the last includes a `subject_hash` |
| `outcome` | `success`, `failure`, `no_op` |
| `reason_code` | Fixed codes listed below |
| `phase` | Optional fixed lifecycle checkpoint; no free-form diagnostics |
| `correlation` | `operation_hash`; Temporal observations also include `workflow_hash`, `run_hash`, `activity_hash`, and `attempt` |
| `certificate_sha256` | Optional public certificate DER SHA-256 fingerprints; absent for encryption keys |

Reason codes are `accepted`, `completed`, `unchanged`, `operation_failed`,
`identity_not_approved`, `approval_update_failed`, `health_check_failed`,
`verified`, `observed`, `observation_unavailable`, and `node_retired`. Phases are `preflight`,
`renew-node`, `postflight`, `prepare`, `add_secondary`, `promote_primary`,
`rewrite`, `remove_old_keys`, and `finalize`. A reason describes the audited
observation, not a replacement for the workflow or cluster's durable state.

Identifiers outside managed UUIDs are hashed as SHA-256 over the UTF-8 encoding
of `<domain>`, a NUL byte, and the identifier. Domains are `cluster`, `node`,
`actor`, `operation`, `workflow`, `run`, and `activity`. Hashing supports joining
against known identifiers and prevents accidental inclusion of credential-like
caller input. It does not anonymize predictable names against dictionary attacks.
No caller-controlled field or raw error is used as a Prometheus label.

Constructors and sinks validate the allowlist. Fingerprints represent public
certificate bytes only; encryption-key bytes and key fingerprints are excluded.
The manual tool rejects mixed PEM bundles and private-key PEM blocks.

## Recorded observations

- T2 renewal acceptance and health checks, terminal node renewal results,
  postflight verification, aggregate renewal/no-op completion, and failure
  cleanup attempts are recorded from activities. Known unsuccessful legacy
  process exits are recorded before retry, including when a later attempt
  succeeds. Cleanup that observes a matching typed terminal result records that
  node's actual renewed, unchanged, or failed outcome independently of the
  workflow's result. Transport errors, cancellation, legacy receipt-only
  resolution, and an empty, unknown or outcome-less typed status do not by
  themselves establish a node renewal outcome and record no node renewal event.
  The cycle failure event is recorded only after the failure is persisted for
  the owning cycle. The
  retained V1 fallback is audited only from an explicit `RENEWAL_OUTCOME` marker
  (`renewed` or `unchanged`); markerless or unrecognized successful output records
  no node renewal event.
- T2 etcd rotation acceptance, phase entry, completion, and failure observations
  correlate with the owning workflow. A phase `check/accepted` means that the
  checkpoint was entered; it does not prove that the phase's node mutations have
  completed. A final `rotate/success` follows successful completion persistence.
  Retiring a permanently lost control-plane node from a failed rotation records
  `rotate/success` with reason `node_retired`, the node and the failed phase;
  refusals record `rotate/failure` and repeats `rotate/no_op`.
- The kubelet-serving CSR controller records these `action/outcome/reason`
  observations for CSRs whose requestor resolves to a managed node:

  | Observation | Meaning |
  | --- | --- |
  | `check/success/verified` | the CSR matches the managed node's identity |
  | `check/no_op/identity_not_approved` | a CSR was rejected and not approved; this is how a rejected identity appears, not as `failure`. Recorded at most once per node per UTC hour |
  | `approve/success/completed` | the approval was written to the CSR |
  | `approve/failure/approval_update_failed` | writing the approval failed; the CSR is retried |
  | `verify/success/observed` | the CSR carries NVKE's approval, has no `Failed` condition, and the leaf (first) certificate carries the request's public key; recorded with only that leaf's public fingerprint |
  | `verify/failure/observation_unavailable` | the certificate on an NVKE-approved CSR could not be parsed or does not carry the request's public key |

  To find rejected identities, filter on `reason_code =
  'identity_not_approved'`; filtering on `outcome = 'failure'` misses them. CSRs
  from other requestors are logged at debug level but never written to the
  ledger. `verify/success/observed` proves only that the CSR has a public
  certificate for the request's key, not that the kubelet installed it. It
  does not authenticate the issuer: a tenant cluster-admin can write CSR
  status, including the approval condition and the certificate. Because a tenant
  cluster-admin can still create CSRs naming a managed node, CSR-derived writes
  are bounded per cluster and node: at most 12 events within any hour, counted
  in the ledger itself so a controller restart does not reset the bound.
  Rejections are recorded at most once per node per UTC hour: all rejected CSRs
  for that node in that hour share one event, which uses one slot of the bound,
  so repeated rejections cannot crowd out approvals. A valid CSR records three
  events, or four if an approval write fails and is retried, so the bound admits
  about four valid CSRs per node per hour, or three if their approvals are retried.
  Events beyond it are dropped and counted as
  `nvke_controller_credential_audit_writes_total{result="rate_limited"}`. A
  non-zero `rate_limited` means CSR-derived evidence for some node and hour,
  including approvals and issued certificates, may be missing from the ledger.
  Investigate it. The metric carries no node label: find nodes with 12
  `kubelet_serving` CSR events within one hour in the ledger (any `approve` or
  `verify` event, and `check` events with `reason_code` `verified` or
  `identity_not_approved`), then
  reconcile that node's CSRs in the tenant cluster with the ledger.
- Independent background certificate inspections record a control-plane leaf
  bundle check and a kubelet-serving check with available public fingerprints.
  These observations have no renewal workflow attribution. An observation that
  repeats the previous inspection's outcome, reason and fingerprints for the
  same node and credential is not recorded again once the previous observation
  was recorded, so an unchanged node adds no rows. That includes a probe that
  keeps failing the same way, such as a node agent without the inspection RPC:
  only its first failure is recorded. If the ledger write for an inspection
  fails, the next inspection is recorded even when unchanged. Changes,
  including a change of failure reason, are recorded. A failed probe emits
  unavailable evidence without claiming retained older fingerprints are fresh;
  a successful read does not prove renewal occurred or that a certificate is unexpired.
- T1 events are explicit operator observations accompanying the
  [site-management certificate renewal runbook](../../docs/site-operations/certificate-renewal.md).
  NVKE does not automatically run, approve, or renew management-plane credentials.

Effects occur in activities and reconcilers, never workflow replay code. Activity
retry attempts remain separate observations because `attempt` contributes to
`event_id`; repeated writes of an identical observation preserve the first row
through `ON CONFLICT DO NOTHING`. Distinct outcome, phase, fingerprint or execution
correlation produces a different ID. Replaying workflow history does not execute
completed activities or emit their audit observations again. An activity that
actually runs again after reset can emit another observation with a new run hash.

## Actor attribution

CSR approval identifies the automatic NVKE controller. Renewal workflows matching
the scheduler's exact due-cycle ID convention are classified as automatic;
this is a source convention, not authenticated human attribution. A caller
with permission to start Temporal workflows can choose that ID. Other renewal
and rotation workflow invocations use `operator_unattributed`: direct Temporal
invocation does not convey an authenticated operator subject to these activities.
Correlate the run with the site's Temporal access logs when human attribution is
required. No operator subject is inferred from a worker process identity or
supplied workflow ID text.

The T1 tool hashes the operator subject provided with `--actor` and records
`operator_reported`. This is self-report. Bind exported evidence to the site's
operator authentication and change approval records; the tool itself does not
verify them. An `approve` observation records a stated approval, not enforcement
of the operator's permissions.

## Sink and failure behavior

Committed T2 records are durable to the guarantees of the site's PostgreSQL and
backups. They are retained independently of cluster deletion. There is no external
exporter, delivery acknowledgement from a log collector, or automatic pruning.
Structured controller logs report sink failure with a fixed code; they are not
the durable audit sink.

Writes occur after lifecycle state changes and are **bounded best effort** with a
separate two-second context, including during cancellation cleanup. Sink failure
does not change a successful operation result, advance a failed operation's
schedule, or release/reacquire its mutation lock. A database outage or process
crash between state/mutation and audit insertion can leave an evidence gap. CSR
approval and PostgreSQL insertion are separate transactions. Failed writes are
not queued for background retry, and a successfully completed activity is not
retried solely for missing audit evidence. Do not infer complete coverage or
exactly-once delivery from this ledger.

`nvke_controller_credential_audit_writes_total{result="stored"}` counts successful
write attempts, including duplicate inserts. `result="sink_failed"` counts sink
failure; `result="invalid_event"` identifies rejected construction;
`result="rate_limited"` counts CSR-derived events dropped by the per-node bound. All
three mean that ledger evidence is missing. A useful site alert expression is:

```promql
sum(increase(nvke_controller_credential_audit_writes_total{result=~"sink_failed|invalid_event|rate_limited"}[10m])) > 0
```

When it fires for `sink_failed` or `invalid_event`, check controller database
access, migration availability, and recent restarts. For `rate_limited`, find
the node that reached the CSR bound, as described above. Reconcile workflow
histories, cluster conditions and explicit operator evidence for the affected interval. Do not replay mutations merely to
manufacture missing events. Each result series exists at zero from process
start, so failures after the first scrape of a process are visible to
`increase()`. A burst between process start and the first scrape, or a
controller restart in the middle of a burst, can still be missed, as can
controller or scrape outages; reconcile that interval as above and use the
site's controller and scrape availability alerts.

## Manual T1 evidence

Build the tool into a new private directory before entering the maintenance
window. `mktemp -d` creates a directory only the current user can access, so no
other user can pre-create the binary or the evidence file. The chain stops if
the directory or the build fails. Reuse `$evidence_dir` for later observations
in the same shell:

```bash
cd controller
evidence_dir="$(mktemp -d)" &&
  go build -o "$evidence_dir/credential-audit" ./cmd/credential-audit
```

Record each observation only after you performed that step. For example, after
the pre-renewal checks of the first node passed:

```bash
"$evidence_dir/credential-audit" \
  --plane t1 --credential kubeadm_leaf_bundle --action check \
  --cluster example-management-cluster --node example-control-plane-1 \
  --operation example-maintenance-run/check-1 --actor example-operator \
  --outcome success --reason verified >> "$evidence_dir/credential-evidence.jsonl"
```

Use a stable run identifier with a per-step suffix and emit an observation
after each check, operator approval, renewal, and verification. Use `--outcome failure --reason
operation_failed` when a step fails, and `--outcome no_op --reason unchanged` when
inspection proves there is no change. Use `--action approve --outcome success
--reason accepted` to record a separate operator approval. Optional `--timestamp`
accepts RFC3339 for recording an observed step time. Optional `--phase` selects a
fixed checkpoint.

`event_id` excludes the timestamp so that a retried write of the same observation
deduplicates. Two observations with identical flags, apart from `--timestamp`,
therefore get the same `event_id`, and export deduplication merges them. Give each
step its own `--operation` suffix (for example `example-maintenance-run/renew-2`)
whenever any step, including a renewal or approval, is repeated with otherwise
unchanged fields.

When the [rotation demo](https://github.com/dsx-ai-factory/nvke/blob/main/hack/rotation-demo/README.md) observes T1
maintenance, its collector also requires `--cluster` to be the exact
`kube-system` namespace UID of the site-management cluster (the example above
uses a placeholder name), `--node` to be the configured node name (not
`--node-id`), and `--operation` to be its configured
`manual_operation` or that value with a per-step suffix `/<action>-<n>`, for
example `example-maintenance-run/renew-2`, where `<action>` is `check`,
`approve`, `renew`, `rotate` or `verify` and `<n>` is 1 through 99 without
leading zeros. Rows with a different cluster or operation fail with
`manual_audit_target_or_run_mismatch`. A row recorded with `--node-id` or a
different node name does not count toward that node, and the run fails with
`per_node_audit_coverage_incomplete`. Keep the original audit records; do not
delete rows or repeat maintenance to manufacture matching evidence.

For public certificate evidence, add `--certificate /path/to/public-certificate.pem`
to `check` and `verify` observations. Maximum input is 1 MiB and 32 certificates.
Take public certificate-only copies without exporting private keys or kubeconfig
contents. Record before/after fingerprints with the runbook's
[health checks](../../docs/site-operations/certificate-renewal.md#1-select-a-site-and-prepare-maintenance)
and [post-renewal verification](../../docs/site-operations/certificate-renewal.md#4-verify-before-the-next-node);
a changed fingerprint alone does not prove availability or CA stability.

`--cluster-id` and `--node-id` accept managed UUIDs instead of hashed external
identifiers. Stdout is one JSON line; schema/input/output failure exits nonzero.
The tool neither writes PostgreSQL nor contacts a site. Secure, retain and export
the JSONL alongside the sanitized maintenance evidence. Shell redirection alone
does not provide durable storage, access controls, immutable evidence or delivery
acknowledgement. Detect and handle tool errors before marking evidence complete.

## Export and retention

Use a read-only database role to select/export the JSON events. For example,
with the site's already-configured PostgreSQL client connection:

```sql
SELECT observed_at, recorded_at, event
FROM credential_audit_events
WHERE event #>> '{target,cluster_id}' = '00000000-0000-0000-0000-000000000001'
ORDER BY observed_at, event_id;

COPY (SELECT event::text FROM credential_audit_events
      ORDER BY observed_at, event_id) TO STDOUT;
```

Export into the site's access-controlled evidence store with transport integrity
and backup/retention policies. Deduplicate combined exports by `event_id`; do not
count raw rows from repeated exports as distinct operations. Do not export
cluster records, Temporal payloads or Kubernetes Secret objects as audit evidence.
Those stores can contain private material even though the audit schema does not.

Set a retention interval covering Temporal workflow retention, incident response,
release qualification, and the site's evidence requirements. There is no default
expiration. Unchanged inspections, repeated identical inspection failures and
CSR churn are bounded as described above, so growth follows credential changes,
new failures and operations; still monitor
table size. Verify exported records and backups before any
operator-controlled deletion, and keep unresolved operation/recovery evidence.
Pruning rows removes the deduplication record too; a delayed repeated write can
then reappear. This alpha ledger is not immutable against a database administrator
and has no tamper-evident signing chain.

## Qualification

Unit tests exercise allowlist validation, public certificate parsing, stable event
IDs, ledger conflict handling, activity correlation, operator schema compatibility,
and sink failures without changing renewal/CSR results. Qualify PostgreSQL
migration/export, real Temporal activity retry/reset behavior, site access
controls and retention, and event coverage during live T1/T2 procedures before
claiming release evidence. No live cluster run is implied by the unit tests.
