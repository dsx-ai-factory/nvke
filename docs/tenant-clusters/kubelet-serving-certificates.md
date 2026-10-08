# Kubelet serving certificate renewal and troubleshooting

T2 kubelets in NVKE's kubeadm profile use `serverTLSBootstrap: true`. They request
serving certificates through `kubernetes.io/kubelet-serving` CSRs. The CWE tenant
controller approves only requests matching its managed node identity; Kubernetes'
signer issues the certificate and the kubelet installs it. This runs separately
from `RenewK8sCertsV2`, which renews control-plane kubeadm leaves.

Use the default `/var/lib/kubelet/pki/kubelet-server-current.pem` layout and
maintain working kubelet-to-API authentication, tenant-manager access, approval
RBAC and Kubernetes signer configuration. Custom certificate locations,
self-signed kubelet serving configurations and unmanaged T1 nodes are not this
procedure's profile. Never remove a production serving certificate just to make
a request appear; deliberate early-rotation tests belong in a selected
qualification environment with node recovery access.

## Observe a real request

On the operator machine, select the tenant and expected node from inventory:

```bash
TENANT_KUBECONFIG=/secure/tenant.kubeconfig
TENANT_CONTEXT=example-tenant-context
NODE=example-worker-1
kubectl --kubeconfig "$TENANT_KUBECONFIG" --context "$TENANT_CONTEXT" \
  get node "$NODE" -o wide
kubectl --kubeconfig "$TENANT_KUBECONFIG" --context "$TENANT_CONTEXT" get csr \
  -o custom-columns='NAME:.metadata.name,SIGNER:.spec.signerName,REQUESTOR:.spec.username,CREATED:.metadata.creationTimestamp,CONDITIONS:.status.conditions[*].type'
```

Confirm node name and InternalIP match CWE's desired state, not just a
Kubernetes Node object. Select a newly observed serving CSR for this node, then
inspect bounded public fields and its signed certificate when issued:

```bash
CSR=replace-with-selected-csr-name
kubectl --kubeconfig "$TENANT_KUBECONFIG" --context "$TENANT_CONTEXT" get csr "$CSR" \
  -o jsonpath='{.spec.signerName}{"\n"}{.spec.username}{"\n"}{.spec.groups}{"\n"}{.spec.usages}{"\n"}{range .status.conditions[*]}{.type}{" "}{.status}{" "}{.reason}{"\n"}{end}'
set -o pipefail
kubectl --kubeconfig "$TENANT_KUBECONFIG" --context "$TENANT_CONTEXT" get csr "$CSR" \
  -o jsonpath='{.spec.request}' | base64 --decode |
  openssl req -noout -verify -subject -text
kubectl --kubeconfig "$TENANT_KUBECONFIG" --context "$TENANT_CONTEXT" get csr "$CSR" \
  -o jsonpath='{.status.certificate}' | base64 --decode |
  openssl x509 -noout -dates -fingerprint -sha256
```

These examples use GNU `base64 --decode`; on macOS use `base64 -D`. Request
subjects/SANs are public identity data; retain them only in the private diagnostic
record. An empty `status.certificate` makes the last command fail until issuance;
that is an incomplete stage, not successful installation.

CWE's identity allowlist requires all of the following:

- Requestor `system:node:<node>` and membership in `system:nodes`.
- Node name present in CWE desired state, with its recorded IP.
- One valid, signed PEM certificate request with subject CN
  `system:node:<node>` and organization `system:nodes`.
- Usage `server auth`, with only optional `digital signature` and
  `key encipherment` alongside it.
- At least one DNS/IP SAN. Every DNS SAN equals the managed node name and every
  IP SAN equals its recorded IP. Email and URI SANs are rejected.

Expected progression is pending, `Approved=True` with reason
`NKEValidatedNodeIdentity`, then a nonempty signed certificate. `Approved` alone
does not establish signing, and signing alone does not establish node
installation. A skipped identity mismatch remains pending; CWE does not deny
it as an automatic correction.

## Prove node installation

On that node, use a privileged session and record only public metadata. The
symlink target holds the kubelet's serving certificate **and its private key**, so
read it locally and never copy it off the node as evidence:

```bash
sudo test -L /var/lib/kubelet/pki/kubelet-server-current.pem
sudo openssl x509 -in /var/lib/kubelet/pki/kubelet-server-current.pem \
  -noout -dates -fingerprint -sha256
sudo grep -E '^(serverTLSBootstrap|rotateCertificates):' /var/lib/kubelet/config.yaml
sudo systemctl is-active kubelet
```

Compare the installed DER fingerprint and NotAfter with the issued CSR
certificate. On a genuine renewal, require a changed fingerprint and later
expiry relative to the baseline. A missing link, different fingerprint, old
expiry or stopped kubelet requires investigation. The inspection RPC reads this
fixed certificate-and-key file locally and returns only public certificate
metadata; it does not mutate kubelet configuration or cause renewal.
Verify fresh `kubelet-serving` inspection observations and node readiness. The
controller refreshes its observation on the inspection interval (one hour by
default), so a newly installed certificate can take that long to appear. On the
node, print the installed NotAfter as Unix seconds (GNU `date`):

```bash
date -u -d "$(sudo openssl x509 -in /var/lib/kubelet/pki/kubelet-server-current.pem \
  -noout -enddate | cut -d= -f2)" +%s
```

Then read the controller's metrics, either with a Prometheus query or directly
as in [Read the management plane metrics](../operate/observe.md#read-the-management-plane-metrics).
With the metrics port forwarded on the operator machine, set `CLUSTER` to the
workload cluster name and `NODE` as above:

```bash
CLUSTER=example-tenant
curl -s http://127.0.0.1:9090/metrics | awk -v cluster="$CLUSTER" -v node="$NODE" '
  /^nvke_controller_certificate_(observation_available|not_after_timestamp_seconds)\{/ &&
  index($0, "credential=\"kubelet-serving\"") &&
  index($0, "tenant_cluster=\"" cluster "\"") && index($0, "node=\"" node "\"") {
    printf "%s %.0f\n", $1, $NF
  }'
```

Require `certificate_observation_available` to be `1` and
`certificate_not_after_timestamp_seconds` to equal the installed NotAfter above.
A value of `0`, a missing series or an older NotAfter means the controller still
reports the previous certificate or no fresh observation; wait for the next
inspection and investigate if it persists. Then check node readiness and the
tenant API:

```bash
kubectl --kubeconfig "$TENANT_KUBECONFIG" --context "$TENANT_CONTEXT" \
  wait --for=condition=Ready "node/$NODE" --timeout=60s
kubectl --kubeconfig "$TENANT_KUBECONFIG" --context "$TENANT_CONTEXT" \
  get --raw='/readyz?verbose'
```

The node file establishes installed material. Additionally, on that node verify
the actual kubelet endpoint with the tenant CA and the identity the certificate
carries: its inventory IP for an IP SAN, or the managed node name for a DNS-only
SAN (the CSR output above shows which). The connection always goes to the
inventory IP. This example uses the default kubelet port and GNU `timeout`;
retain TLS errors and require both pipeline commands to succeed:

```bash
NODE_IP=192.0.2.10  # Replace with this node's managed InternalIP.
NODE_NAME=example-worker-1  # Replace with this node's managed name.
VERIFY_ARGS=(-verify_ip "$NODE_IP")
# For a DNS-only SAN, use instead: VERIFY_ARGS=(-verify_hostname "$NODE_NAME")
NODE_CONNECT_HOST="$NODE_IP"
case "$NODE_IP" in *:*) NODE_CONNECT_HOST="[$NODE_IP]" ;; esac
set -o pipefail
sudo timeout 10s openssl s_client -connect "$NODE_CONNECT_HOST:10250" \
  -CAfile /etc/kubernetes/pki/ca.crt "${VERIFY_ARGS[@]}" \
  -verify_return_error </dev/null |
  openssl x509 -noout -dates -fingerprint -sha256
```

Require a valid CA check with the matching IP or hostname, and the same public
fingerprint as the issued and installed certificate. An old endpoint certificate, trust failure, connection
failure or timeout is incomplete verification. Do not bypass TLS verification
in a production acceptance check. Nondefault ports/trust layouts need their own
validated endpoint probe.

## Failures and recovery

| Observation | Check and recovery |
| --- | --- |
| No new serving CSR near expiry | Check installed expiry, `serverTLSBootstrap`, kubelet client authentication, API reachability and kubelet logs. Repair the supported kubelet profile; do not confuse a client CSR signer with the serving signer. |
| Pending CSR with identity skip | Check CWE desired node name/IP against requestor, subject, usages and SANs. Correct the actual inventory/bootstrap problem, let kubelet request valid material, and verify a new request. Do not approve the mismatched request manually. |
| Pending valid CSR with no decision | Check tenant-manager access, controller health and approval RBAC. Restore that access; CWE can reconcile pending requests. Preserve the failed interval for alert/audit evidence. |
| Approved but certificate empty | Check Kubernetes controller-manager signer availability, signing certificate/key configuration and failure conditions. Restore signing; approval is not the issuing operation. |
| Denied or Failed | Treat the request as terminal; inspect the issuer/approver's recorded reason. CWE will not override denial. Repair the underlying cause and investigate whether kubelet requests new material. |
| Issued but node still uses old certificate | Check kubelet logs, PKI permissions and installed current link. Repair under the site's node recovery procedure; a new serving certificate may not be loaded yet. Keep remaining nodes untouched during manual intervention. |
| Missing/stale CSR metrics | Restore tenant-manager API observation. Cached zero counts do not establish absence of trouble. |

On the node, `sudo journalctl -u kubelet --since '30 minutes ago'` supplies local
diagnostics. On the management cluster, use
[controller and Temporal diagnostics](../operations/observability.md). Logs and
CSR names may contain sensitive operational context; keep raw diagnostics
private and export only reviewed fixed reason/outcome evidence. Do not delete
terminal CSR objects solely to hide a failed/denied inventory alert; follow the
site's evidence-retention and Kubernetes cleanup policy after recovery.

The [monitoring guide](../../controller/docs/credential-monitoring.md#alert-responses)
defines stale, failed and unavailable CSR alerts. Counts are current object
inventory; an approved request without a signed certificate still counts as
pending. The [audit guide](../../controller/docs/credential-audit.md#recorded-observations)
records identity/approval and observed signing results; `verify/observed` alone
does not claim the kubelet installed its certificate. Correlate node-side public
checks with those events and retain any observation gap.

For release acceptance, observe an actual kubelet's request, approval, issuance
and installation through a controlled expiry/renewal run, preserve before/after
public fingerprints and freshness/alert recovery, and record target identity,
versions and traffic impact. Include the monitoring evidence required by the
[live acceptance checklist](../../controller/docs/credential-monitoring.md#offline-verification-and-live-acceptance):
the warning-window rule firing, receipt by the configured receiver, resolution
after renewal, and unavailable-inspection and stale or failed CSR alerts. Mocked
CSRs do not replace that qualification.
