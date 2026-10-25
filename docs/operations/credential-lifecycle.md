# OSS alpha credential operations

Use this guide to choose the supported renewal or rotation procedure, then follow
its complete runbook. **T1** is the site-management Kubernetes cluster hosting
the NVKE controller, Temporal and PostgreSQL. **T2** is a controller-managed tenant
Kubernetes cluster.
Sharing a kubeadm layout does not make their maintenance interchangeable: a T1
restart can interrupt the services that orchestrate T2 maintenance.

The supported alpha profiles use kubeadm static control-plane pods, stacked local
etcd, the default `/etc/kubernetes` and `/var/lib/kubelet/pki` paths, and existing
local kubeadm CAs. These names describe implementation requirements; they are
not config-profile filenames. Fleet profiles are supplied by the deployer, so
verify the actual node layout before selecting a procedure.

## Credential and implementation inventory

| Credential | Plane and owner | Supported action and evidence |
| --- | --- | --- |
| API server, API-to-kubelet, front-proxy client and API-to-etcd leaf certificates | T1 operator; T2 controller workflow and node agent | T1 manual or T2 scheduled/forced kubeadm leaf renewal. Compare expiry and public fingerprints; restart and verify consumers. |
| Stacked-etcd server, peer and healthcheck-client leaf certificates | T1 operator; T2 controller workflow and node agent | Renew in the same kubeadm leaf bundle. Verify actual etcd membership and every endpoint before proceeding. |
| `admin.conf`, `controller-manager.conf`, `scheduler.conf`; `super-admin.conf` where present | T1 operator; T2 controller workflow and node agent | Renew embedded client certificates with the bundle. `super-admin.conf` is optional on joined control planes. Exported admin copies require a separate secure refresh. |
| Kubernetes, front-proxy and etcd CA certificates and private keys | T1/T2 bootstrap and site PKI owner | Existing self-signed CAs remain in place during leaf renewal. Public CA expiry is observed on managed nodes; CA rotation is deferred. |
| Kubelet serving certificate | T2 kubelet, controller tenant CSR approver, Kubernetes signer | Kubelet requests renewal; the controller validates managed node identity before approval; Kubernetes signs; kubelet must install the result. Check each stage separately. |
| Kubelet client certificate | Kubeadm/kubelet and Kubernetes client signer | Fixed-path public expiry inspection on joined T2 nodes. The leaf-bundle workflow does not renew it; investigate kubelet client/bootstrap configuration separately. |
| Etcd API-object data-encryption key | T2 operator-initiated controller workflow and node agents | Existing secretbox key rotation: distribute, promote, rewrite old objects, remove old keys, finalize. Prove preexisting-object rewrite and all-node convergence. |
| T1 etcd data-encryption key | Site backup/recovery owner | Preserve the matching key with protected recovery backups. NVKE provides no T1 key-rotation workflow. |
| SPIFFE SVIDs, SPIRE CA material and agent transport identity | Site identity deployment | Owned by SPIRE/transport setup; not renewed by kubeadm certificate maintenance. See [agent security](https://github.com/dsx-ai-factory/nvke/blob/main/agent/docs/SECURITY.md) and [transport configuration](https://github.com/dsx-ai-factory/nvke/blob/main/controller/docs/configuring-node-agent-tls-jwt.md). |
| Service-account signing keys, bootstrap tokens, disk-encryption keys, OIDC issuer keys, application TLS and external PKI | Their component/site owners | Outside these rotation procedures. Their presence does not imply an alpha rotation API. |

For the exact public inspection catalog and unavailable/optional behavior, see
[Credential monitoring](../../controller/docs/credential-monitoring.md). Private
keys, encryption keys and arbitrary file paths never form metric labels.

## Select a procedure

| Task | Runbook | Completion requirement |
| --- | --- | --- |
| Renew T1 kubeadm leaves | [Site-management certificate renewal runbook](../site-operations/certificate-renewal.md) and [additional evidence requirements](#manual-t1-evidence-and-qualification) | One control-plane node at a time; all four static components recovered, unchanged CAs, site services healthy. |
| Scheduled or forced T2 kubeadm renewal | [Tenant certificate renewal](../tenant-clusters/certificate-renewal.md) | Terminal successful owning workflow and cluster state, node health, renewed leaves or documented no-op, and unchanged CAs, verified on each node. |
| Diagnose T2 kubelet-serving renewal | [Kubelet serving certificates](../tenant-clusters/kubelet-serving-certificates.md) | Identity-valid CSR approved, certificate issued, installed public certificate verified on the node. |
| Rotate an existing T2 etcd encryption key | [Etcd key rotation](../../controller/docs/etcd-encryption-key-rotation.md) | Requires Kubernetes 1.34+ and an agent from the same qualified build as the controller; see its loaded-config/process-metrics prerequisites before use. Preexisting Secret rewritten/readable, every API server uses the new primary, pending state and ownership cleared. |
| Install or respond to lifecycle alerts | [Credential monitoring](../../controller/docs/credential-monitoring.md) | Fresh public observations, selected scrape/rules, evaluated alerts and delivered notification. |
| Record, export or recover audit evidence | [Credential audit](../../controller/docs/credential-audit.md) | Validated manual JSONL or committed T2 ledger records, access-controlled export and declared evidence gaps. |

Run one cluster mutation at a time. Keep control-plane membership fixed during
T2 renewal/rotation; do not overlap upgrades, recycling, node replacements or
another credential operation. Before T1 maintenance, pause fleet changes and
finish or explicitly recover outstanding T2 work. A maintenance window must
include console access, a recovery owner, protected backups and the agreed
outage/error budget.

## Topology and availability

A healthy three-control-plane cluster with working VIP/load-balancer failover is
the HA qualification baseline. Three Ready Kubernetes nodes alone prove neither
etcd quorum nor client failover. Test the actual endpoint clients use, and check
etcd voters and every endpoint. T2 leaf renewal refuses unstable membership and
never renews a two-control-plane cluster; scale such a cluster up to three or
more control-plane nodes before its certificates expire. Renewal resumes only
once `controlPlaneConfig.desiredCount` is 3 or more. For other multi-node topologies, retain qualification
evidence before advertising availability guarantees.

A single control plane has no redundancy. Its leaf renewal restarts its only
etcd and API server, causing an expected API interruption; plan an outage. Etcd
key rotation hot-reloads configuration and is a different operation, but a
single-node site still has no failure tolerance. External etcd, external CA
signing and custom PKI layouts need separate procedures.

`RenewK8sCertsV2` is the canonical T2 leaf workflow. The registered
`RenewK8sCerts` type preserves old history replay. The receipt-aware embedded
runbook is an executor compatibility path selected by V2; it is not a separate
operator maintenance API. Legacy alternate T2 deployment paths are not claimed
as qualified alpha profiles. Do not bypass health or ownership gates by invoking
old workflows or editing manifests manually.

## Self-signed CA behavior and deferred capabilities

Kubeadm bootstrap creates the Kubernetes, front-proxy and stacked-etcd CAs when
the profile supplies no existing PKI. Leaf renewal uses those current CAs and
does not extend their validity or replace their trust roots. Record public CA
fingerprints before and after every leaf procedure; any difference is an
unexpected change requiring investigation. A renewed leaf cannot compensate for
an expired or untrusted CA.

Approaching CA expiry requires a separately reviewed PKI migration or cluster
replacement plan. This alpha feature does not provide CA rotation, revocation,
customer-managed PKI, a general break-glass recovery for already expired PKI, or
full automatic lifecycle management. Neither deleting a local credential copy
nor renewing it revokes other copies. Do not advertise those capabilities based
on a successful leaf renewal.

## Monitoring and audit during maintenance

Confirm inspection freshness before relying on expiry. A renewal due date is a
check schedule, not an expiry estimate. Monitoring covers database-managed nodes, those recorded in the controller database;
unmanaged T1 nodes need the manual runbook's public checks. Missing/stale
observations and a failed scrape must remain visible, rather than interpreted as
healthy credentials.

The [monitoring guide](../../controller/docs/credential-monitoring.md#configuration)
is the source of configuration defaults/bounds and shipped alert names. Install
its optional `PrometheusRule` only with a selected Prometheus Operator-compatible
evaluator and a real Alertmanager receiver; NKTP scraping by itself does not
deliver an alert. Retain a warning-before-expiry, delivered-notification and
post-renewal-resolution sample from the site.

Use the [audit guide](../../controller/docs/credential-audit.md#manual-t1-evidence)
to prepare `credential-audit` before T1 maintenance. Record `check`, `approve`,
`renew` and `verify` observations at actual step boundaries, with a stable run ID
and distinct step IDs. The audit tool's operator subject is self-reported; tie
it to the site's authenticated change record. When the
[rotation demo](https://github.com/dsx-ai-factory/nvke/blob/main/hack/rotation-demo/README.md)
observes the maintenance, pass the exact `kube-system` namespace UID as
`--cluster`, the configured node name as `--node` (not `--node-id`), and its
configured `manual_operation`, optionally with a per-step
suffix `/<action>-<n>` such as `/renew-2`, as `--operation`, where `<action>` is
`check`, `approve`, `renew`, `rotate` or `verify` and `<n>` is 1 through 99
without leading zeros; rows with another cluster or operation fail with
`manual_audit_target_or_run_mismatch`. A row recorded with `--node-id` or a
different node name does not count toward that node, and the run fails with
`per_node_audit_coverage_incomplete`. T1 output is local JSONL, not
automatically inserted into PostgreSQL. For automatic T2 observations, export
the controller's ledger with a read-only database role and deduplicate by
`event_id`.

T2 writes are bounded best effort after mutations, so a database outage or crash
can leave gaps. Direct Temporal starts do not supply an authenticated human actor.
The audit guide documents both attribution and retention limits. Monitor
`nvke_controller_credential_audit_writes_total` failures, reconcile missing events
with reviewed workflow evidence, and do not repeat a mutation to manufacture an
audit record.

## Manual T1 evidence and qualification

The manual renewal procedure is the separate
[site-management certificate renewal runbook](../site-operations/certificate-renewal.md);
this section adds evidence requirements, not a replacement renewal/restart or
recovery procedure. Follow
that complete runbook's node-by-node ordering, backups, quorum, health and
stop-on-failure gates. T1 maintenance can interrupt the audit database itself.

Prepare the audit tool before the window. Record actual preflight, approval,
each node's renewal and postflight as distinct observations under the maintenance
run, following the [manual audit contract](../../controller/docs/credential-audit.md#manual-t1-evidence).
Check its exit status and append only validated JSONL to a protected evidence
file. These records are self-reported, not automatic T1 orchestration or
PostgreSQL delivery. Keep original records and declare gaps; never repeat
maintenance merely to manufacture evidence.

On **each selected node**, add public CA checksums and leaf expiry/fingerprints
to the runbook's preflight record. These commands require `sha256sum` and
`openssl`; they read no private key:

```bash
CA_FINGERPRINTS_BEFORE=$(sudo sha256sum /etc/kubernetes/pki/ca.crt \
  /etc/kubernetes/pki/front-proxy-ca.crt /etc/kubernetes/pki/etcd/ca.crt) &&
  printf '%s\n' "$CA_FINGERPRINTS_BEFORE" &&
  sudo openssl x509 -in /etc/kubernetes/pki/apiserver.crt \
    -noout -dates -fingerprint -sha256 ||
  { echo 'Preflight failed; stop and investigate.' >&2; false; }
```

After that node's renewal and consumer verification, compare against the same
node's retained baseline. If the session was lost, restore the recorded
pre-renewal checksums; never substitute an after-renewal baseline:

```bash
(
  set -eu
  : "${CA_FINGERPRINTS_BEFORE:?Restore the recorded pre-renewal CA checksums for this node.}"
  CA_FINGERPRINTS_AFTER=$(sudo sha256sum /etc/kubernetes/pki/ca.crt \
    /etc/kubernetes/pki/front-proxy-ca.crt /etc/kubernetes/pki/etcd/ca.crt)
  [[ "$CA_FINGERPRINTS_AFTER" == "$CA_FINGERPRINTS_BEFORE" ]] || {
    echo 'Unexpected CA change; stop maintenance and investigate.' >&2
    exit 1
  }
  sudo openssl x509 -in /etc/kubernetes/pki/apiserver.crt \
    -noout -dates -fingerprint -sha256
)
```

Require identical CA file checksums and changed renewed leaf fingerprints with
later NotAfter values. Check all renewed entries with
`sudo kubeadm certs check-expiration`, not just this API-server sample. CA file
checksums here are public file comparisons; the audit tool separately records
public DER fingerprints. Neither extends CA validity nor qualifies availability.

Retain source/tool revisions, Kubernetes version, layout/topology, UTC times,
per-node preflight/renew/restart/postflight outcomes, unchanged CA checksums,
leaf expiry/fingerprints, API outage summary, protected backup references and
audit references. Record failed and unexecuted checks explicitly. Bind a live
qualification claim to its original evidence, not a local syntax or docs check.

## Evidence and secret handling

Use the [credential qualification demo](https://github.com/dsx-ai-factory/nvke/blob/main/hack/rotation-demo/README.md) for
explicitly selected T2 renewal/key-rotation scenarios, expiry alert delivery,
and separate T1 runbook observation. It records required failures and missing
scenarios in sanitized release evidence; offline tests do not qualify a site.

Keep a sanitized maintenance record with source revisions, controller/agent and
Kubernetes versions, profile/layout, cluster and node identities, topology,
UTC start/end times, workflow/run IDs or manual step IDs, public expiry and
fingerprints, health gates, API traffic outcomes, alerts and audit references.
State which checks failed or were skipped; a workflow start acknowledgement is
not completion, and an offline mock test is not live qualification.

Keep private backup artifacts separate: kubeconfigs, CA keys, node PKI archives,
etcd snapshots and their matching encryption keys belong in encrypted,
access-controlled storage outside Git. Do not paste Secrets, key bytes,
certificate PEM, kubeconfig contents, arbitrary command output or full Temporal
histories into Jira/PR/demo evidence. Public DER certificate SHA-256 fingerprints
are sufficient for certificate comparisons. Encryption-key proof must use the
trusted verifier described in the rotation guide; never export key bytes.

Verify every run against the selected site before declaring release acceptance.
These guides and offline checks provide executable procedures, not a claim that
an environment has passed them. Preserve live results and known gaps with the
[release qualification record](https://github.com/dsx-ai-factory/nvke/blob/main/docs/qualification/overview.md).
