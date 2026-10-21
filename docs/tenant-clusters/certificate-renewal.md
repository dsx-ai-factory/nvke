# Renew tenant Kubernetes leaf certificates

This is the operator flow for the NVKE controller's canonical `RenewK8sCertsV2`
workflow.
Scheduled and forced cycles use the same health, executor and ownership gates.
The [implementation and failure contract](../../controller/docs/certificate-renewal.md)
describes receipt-aware fallback, static-pod restoration and retained-lock
behavior. Use the separate
[site-management certificate renewal runbook](../site-operations/certificate-renewal.md)
for the site-management cluster.
See the [additional manual evidence requirements](../operations/credential-lifecycle.md#manual-t1-evidence-and-qualification)
for audit and public CA/leaf comparisons.

## Prerequisites and target selection

Use a provisioned controller-managed T2 cluster with kubeadm static pods, stacked local
etcd, default PKI paths and healthy, nonexpired local CAs. Freeze control-plane
membership and arrange a maintenance owner, console access and recovery backups.
All control-plane nodes must be joined/Ready, their agents reachable, all etcd
members healthy and all local APIs ready. Upgrade agents to the revision qualified
for this workflow; reaching the gRPC port alone does not establish support for
V2 renewal and, for clusters with more than one control plane, kube-vip
drain/restore.

The HA baseline is three control planes with a working VIP/load balancer.
Renewal runs on clusters with three or more control-plane nodes, or on a cluster
created with a single control plane. A cluster with exactly two is never
renewed: its cycle is recorded as failed with phase `two-control-plane-voters`. Scale it up to three or more control-plane nodes
before its certificates expire; renewal resumes only once
`controlPlaneConfig.desiredCount` is 3 or more. A two-control-plane cluster
cannot be scaled down to one. A single control plane is allowed with an
expected API outage. Verify the client's actual endpoint and planned outage/error budget.
Do not schedule simultaneous upgrades, node replacements, encryption rotation
or other cluster mutations.

On the operator machine, select the site and retrieve a private tenant kubeconfig:

```bash
umask 077
SITE_NAME=example-site
CWE_URL=https://controller.example.invalid
SITE_KUBECONFIG=/secure/site.kubeconfig
CLUSTER_NAME=example-tenant
CLUSTER_ID=00000000-0000-0000-0000-000000000001
TENANT_KUBECONFIG=/secure/tenant.kubeconfig
TENANT_CONTEXT=example-tenant-context
CWE_CURL_CONFIG=/secure/controller-curl.conf

nvkectl config set-site --name "$SITE_NAME" --url "$CWE_URL" \
  --kubeconfig "$SITE_KUBECONFIG"
nvkectl get kubeconfig "$CLUSTER_NAME" --output-file "$TENANT_KUBECONFIG"
kubectl --kubeconfig "$TENANT_KUBECONFIG" --context "$TENANT_CONTEXT" \
  get namespace kube-system -o jsonpath='{.metadata.uid}{"\n"}'
kubectl --kubeconfig "$TENANT_KUBECONFIG" --context "$TENANT_CONTEXT" get nodes -o wide
kubectl --kubeconfig "$TENANT_KUBECONFIG" --context "$TENANT_CONTEXT" \
  -n kube-system get pods -o wide
kubectl --kubeconfig "$TENANT_KUBECONFIG" --context "$TENANT_CONTEXT" get --raw='/readyz?verbose'
```

Replace every example with inventory values and use a context that exists in the
retrieved kubeconfig. Confirm the `kube-system` UID, node set and API endpoint
match the selected tenant. Expected output is Ready nodes, healthy control-plane
pods and successful `/readyz` checks. Stop on unexpected identity or health.
`nvkectl get kubeconfig` must read the management-cluster access Secret and the
operator must separately have network reachability to the tenant API.

Use an existing mode-0600 curl configuration containing the site's authentication
and trust settings; see [Call the API](../operate/api-authentication.md#call-the-api).
Do not enable curl tracing or print that configuration. Read only the relevant
operational cluster fields for private diagnosis, rather than exporting the
complete cluster object. Review status diagnostics before including them in
sanitized evidence:

```bash
set -o pipefail
curl --fail --silent --show-error --config "$CWE_CURL_CONFIG" \
  "$CWE_URL/api/clusters/$CLUSTER_ID" |
  jq '{id, name, nextCertRenewalDue, conditions: {
    provisioned: .conditions.provisioned, deleting: .conditions.deleting,
    updateInProgress: .conditions.updateInProgress,
    certificateRenewal: .conditions.certificateRenewal}}'
```

`provisioned.status` must be `"True"`, deletion must be absent or have status
`"False"`, and `updateInProgress` absent/null. Condition statuses are the
case-sensitive strings `"True"`, `"False"` and `"Unknown"`; an unknown deletion
status is not an inactive deletion. A past due time is a scheduled check overdue; it
does not state certificate expiry. An existing accepted or failed cycle requires
inspection before another operation.

On **each** control-plane node, use a privileged session to record the baseline:

```bash
sudo kubeadm version -o short
sudo kubeadm certs check-expiration
sudo sha256sum /etc/kubernetes/pki/ca.crt \
  /etc/kubernetes/pki/front-proxy-ca.crt /etc/kubernetes/pki/etcd/ca.crt
sudo openssl x509 -in /etc/kubernetes/pki/apiserver.crt \
  -noout -dates -fingerprint -sha256
sudo crictl --runtime-endpoint unix:///run/containerd/containerd.sock ps \
  --label io.kubernetes.pod.namespace=kube-system
```

Record the public output and all four component container IDs. Verify local
`admin.conf`, static-pod readiness and actual etcd membership/endpoint health with
the [node preflight commands](../site-operations/certificate-renewal.md#1-select-a-site-and-prepare-maintenance).
Select only the tenant node's stacked-etcd container; do not confuse it with a
different etcd instance. Confirm the three local CA keys are available for this
profile. Stop for missing/expired/external PKI, learners, membership changes or
an unhealthy endpoint. Secure the etcd snapshot and corresponding keys before
mutation using the site's backup process.

## Scheduled checks

`certRenewal.controllerEnabled: true` in the controller ConfigMap
(`controllerConfig.name`, default `nvke-controller-config`) enables
scheduling. Defaults are a twelve-hour `certRenewal.interval` and thirty-day
`certRenewal.thresholdDays`. Use the
[validated configuration table](../../controller/docs/credential-monitoring.md#configuration)
when deploying overrides. Scheduled cycles start when `nextCertRenewalDue` is
due and inspect actual leaf expiry. The first time the controller sees a
cluster without a due time, it only sets one an interval ahead, so a
new cluster is not renewed while it is still settling; that first cycle also
waits for every control-plane node to be ready. A successful check may be `unchanged` when
no renewal is needed; those nodes skip kube-vip drain and renewal dispatch.
The next due time advances only on success. A failed scheduled cycle remains
overdue: released failures retry after a fixed one-hour backoff with a new
workflow ID, while a retained `certificate_renewal` lock halts automatic retry.

Observe the persisted `certificateRenewal.cycleID` and that workflow's state.
Never edit a due time to force a test or reuse scheduler IDs for manual work.
Keep authenticated API traffic running through both scheduled and forced runs,
with the site's normal latency/error monitoring. The
[manual runbook's bounded API monitor](../site-operations/certificate-renewal.md#3-renew-one-control-plane-node)
is a basic fallback; it must use this tenant's explicit kubeconfig/context.

## Forced renewal and terminal observation

Start the workflow in the namespace and task queue the controller's workers
poll: `temporal.namespace` and `temporal.taskQueue` in the controller ConfigMap
(defaults `cloud-workflow-engine` and `cloud-workflow-engine-tasks`). A wrong
queue starts a workflow no worker polls. Pass the Temporal CLI's address and TLS
flags for the site's Temporal frontend; see the
[forced renewal contract](../../controller/docs/certificate-renewal.md#forced-renewal).
The address below is a placeholder. A forced cycle renews the entire leaf
bundle even when the threshold check would have been a no-op.

```bash
TEMPORAL_ADDRESS=temporal.example.invalid:7233
TEMPORAL_NAMESPACE=cloud-workflow-engine         # temporal.namespace
TEMPORAL_TASK_QUEUE=cloud-workflow-engine-tasks  # temporal.taskQueue
RENEWAL_WORKFLOW_ID="operator-k8s-cert-renewal-${CLUSTER_ID}-$(date -u +%Y%m%dT%H%M%SZ)"

temporal workflow start --address "$TEMPORAL_ADDRESS" \
  --namespace "$TEMPORAL_NAMESPACE" --task-queue "$TEMPORAL_TASK_QUEUE" \
  --type RenewK8sCertsV2 --workflow-id "$RENEWAL_WORKFLOW_ID" \
  --id-reuse-policy RejectDuplicate --id-conflict-policy Fail \
  --input "{\"clusterID\":\"$CLUSTER_ID\",\"thresholdDays\":30,\"force\":true}"

temporal workflow describe --address "$TEMPORAL_ADDRESS" \
  --namespace "$TEMPORAL_NAMESPACE" --workflow-id "$RENEWAL_WORKFLOW_ID"
```

Retain the returned workflow and run IDs. `start` acknowledges submission; it
does not establish successful renewal. Repeat `describe` and the filtered REST
read until terminal, inspecting the matching run in Temporal UI for activity
results. A successful cycle reports `succeeded` with `renewed` or `unchanged`, a
completion timestamp, an advanced due time and a released mutation lock. Treat
Temporal failure/cancellation/timeout and mismatched/absent cluster ownership as
unresolved until inspected.

The workflow processes one control-plane node at a time. It drains/restores
kube-vip where present and restarts etcd, API server, controller-manager and
scheduler individually with health checks between components and nodes. V1-only
agents are rejected by the capability-distinct V2 start RPC; V2 can select its
embedded receipt-aware compatibility executor. Do not start the legacy
`RenewK8sCerts` workflow as a manual workaround.

## Verify the actual result

After a terminal successful forced cycle, repeat **each node's** expiration,
CA checksum, leaf fingerprint and CRI observations, and all tenant health checks.
Require later leaf expiry and changed leaf fingerprints, identical CA checksums,
replacement containers for all four control-plane components, healthy exact etcd
membership and every endpoint, local APIs/mirror pods healthy, all nodes Ready,
and traffic within the approved budget. A no-op scheduled cycle should retain
leaf fingerprints and avoid renewal restarts; document that outcome explicitly.

Leaf renewal does not reissue the downloaded `nvke:breakglass` kubeconfig.
`nvkectl get kubeconfig` returns the tenant-access breakglass client certificate,
not a kubeadm leaf. Tenant access reissues it only when its remaining lifetime
drops below `thresholdDays` or the cluster CA changes, and leaf renewal keeps the
CA, so a healthy renewal normally leaves that kubeconfig unchanged; see
[Kubernetes API access](https://github.com/dsx-ai-factory/nvke/blob/main/docs/tenant-clusters/kubernetes-api-access.md). Do not compare kubeconfig
copies to judge renewal. Verify the renewed leaves on each node instead:

```bash
sudo kubeadm certs check-expiration
```

Every renewed leaf must show a later expiry than its baseline, and the CA rows
must be unchanged. Together with the cluster status fields above
(`certificateRenewal.state`, its outcome and completion time, and the advanced
`nextCertRenewalDue`) and the node observations, that is the renewal evidence.
Check that the retrieved kubeconfig still works through the selected endpoint:

```bash
kubectl --kubeconfig "$TENANT_KUBECONFIG" --context "$TENANT_CONTEXT" auth whoami
kubectl --kubeconfig "$TENANT_KUBECONFIG" --context "$TENANT_CONTEXT" get nodes
```

Keep downloaded kubeconfigs out of maintenance evidence and distribute them
only through the site's credential process. Check fresh inspection metrics, any
firing alerts and the
[T2 audit export](../../controller/docs/credential-audit.md#export-and-retention).

## Failure and recovery

Stop subsequent maintenance on any failed node, drain/restore or health gate.
Inspect `failedNodeID`, `phase`, `message`, the original Temporal run and selected
executor. The due time remains unchanged. A failed cycle can retain
`updateInProgress.type=certificate_renewal` when executor termination is
unconfirmed; healthy `/readyz` or a closed workflow does not prove node work and
manifest restoration stopped.

Follow the [component recovery contract](../../controller/docs/certificate-renewal.md#status-and-failure-handling):
confirm the same operation ID is terminal on its selected executor, including
all delayed attempts and restoration, repair the node, and preserve durable
receipt files. There is no public retained-lock release API. The recovery owner
must review terminal evidence before a DBA can release only that failed cycle's
lock under an ownership-checked cluster-row transaction. The workflow ID and
lock type must both still match, and `certificateRenewal.state` must be
`failed`. An unexpected owner means stop and investigate. If the state is still
`accepted`, for example because its workflow was terminated, stop and escalate
without releasing the lock: the scheduler keeps requeuing an accepted cycle
without dispatching it, so releasing the lock does not resume renewal.
Do not use a generic condition-clearing SQL command, replay a successful renewal
to obtain evidence, or delete receipts to unblock work.

Once the failed owner's work is proven stopped, its ownership safely released
and all preflight gates healthy, the controller can retry automatically.
Before waiting for that retry or starting a forced workflow, confirm that the
cluster row's `certificate_renewal` reservation column is empty; the REST API
does not show it. A fenced reservation makes admission refuse both and restart
the one-hour backoff; a reservation left by an older V1 workflow fails the cycle
when it reaches that node. If it stays set, follow the reservation
paragraph of the [component recovery contract](../../controller/docs/certificate-renewal.md#status-and-failure-handling). The
automatic retry needs scheduling enabled (`certRenewal.controllerEnabled`) and
starts no earlier than both one hour after the failure and `nextCertRenewalDue`,
so after a forced cycle that failed before its due time it can wait up to
`certRenewal.interval`. That retry takes the lock, so a forced workflow started
meanwhile is refused. Wait for that automatic retry and inspect its result.
Start a **new** uniquely named forced workflow only after it has finished and
forced semantics are still needed, or, if scheduling is disabled, once the lock
is released.
For expired CA, external PKI or lost recovery keys, preserve backups and use a
separately reviewed recovery plan. This guide provides no automatic PKI recovery.

Record exact revisions, profile/topology, UTC times, every gate and continuous
traffic result, public before/after observations, terminal workflow outcome,
alerts/audit evidence and unexecuted checks. Live qualification is a separate
acceptance run; see [alpha credential operations](../operations/credential-lifecycle.md).
