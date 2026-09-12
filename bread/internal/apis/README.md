# The NKE tenant API

This directory is the tenant API of NKE: the three kinds a tenant sees through `kubectl`, and
the shape the management cluster stores behind them. Everything else in NKE, the API server, the
controllers and the code generator, is downstream of the five hand-written files here.

This branch contains those five files and nothing else: no generated code, no behaviour, no
examples. Every field is one of two things, and the type files say which:

- **A live field.** The SRD calls for it and NKE implements it today, or the Kubernetes API
  convention requires it. It is on the tenant API in this PoC.
- **A placeholder.** The SRD calls for it and NKE does not implement it. It is a commented-out
  field carrying the SRD identifier (`UC-nnn`, `NKE-DS9-nnn`) and a one-line reason. Placeholders
  record a loosely held shape so the name is not designed twice. They are revisited with their
  implementation and need no discussion before then.

Anything in neither category is not in the API: no autoscaling, no kubelet tuning, no tenant
taints, no provider identifiers.

## One package, one access matrix

```
internal/apis/nke/           the hub: one Go struct per kind, EVERY field, per-role access markers on each
        │
        ├──► tenant projection     fields with +nke:tenant=rw|r,   nine-field ObjectMeta   ← the tenant API
        ├──► operator projection   fields with +nke:operator=rw|r, nine-field ObjectMeta   ← the operator API
        └──► internal              every field, real metav1.ObjectMeta                     ← what is stored; controllers only
```

The hub is the single source of truth. Each field says, per role, whether that role can read it,
read and write it, or not see it at all; an unmarked field is controller-only. The two projections
are not nested: operators see all tenant fields but write none of them, and operator-owned fields
are invisible or read-only to tenants. The generator that emits the projections is a separate
change; this package is the contract it consumes.

## The allowlist rule, once

Hub (`cluster.go`, trimmed):

```go
type NKENodeStatus struct {
	// NodePoolName is the metadata.name of the owning NodePool.
	// +nke:tenant=r
	// +nke:operator=r
	NodePoolName string `json:"nodePoolName,omitempty"`

	// Conditions are Ready, Healthy and Retiring.
	// +nke:tenant=r
	// +nke:operator=r
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Internal is the break-fix replacement chain; operator-readable.
	Internal NKENodeInternalState `json:"internal,omitempty"`
}
```

What the tenant sees: the tenant-marked fields, their doc comments, and nothing else.

```go
type NKENodeStatus struct {
	// NodePoolName is the metadata.name of the owning NodePool.
	NodePoolName string `json:"nodePoolName,omitempty"`

	// Conditions are Ready, Healthy and Retiring.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
```

`status.internal` is absent from the tenant type, not masked out of it: a tenant body carrying it
fails strict decoding with a 400, and a `kubectl get` can never return it. The same rule gives the
tenant kinds a nine-field `ObjectMeta` (`name`, `namespace`, `uid`, `resourceVersion`,
`generation`, `creationTimestamp`, `deletionTimestamp`, `labels`, `annotations`): finalizers,
ownerReferences and managedFields do not exist on the tenant surface.

There is no `spec.internal`, and operators do not write `status`: status is the controller's
account of the world. Operator intent (repair requests, validation re-runs, control-plane
operations) lives outside these three kinds; the suggestion in the air is an imperative verb on
the operator API backed by an operator-owned record, with the controller mirroring outcomes into
status here. See "Operator intent" in `doc.go`. No field in this package carries
`+nke:operator=rw`.

## What is live and what is a placeholder

The type files are authoritative; this table is the index.

### NKECluster

| Field | Status | Basis |
|---|---|---|
| `spec.networking{podCIDR,serviceCIDR}` | live, create-only | NKE `network.*` |
| `spec.release{channel,name}` | placeholder | UC-002, UC-005, UC-016. No release concept in NKE; version is a bare string today |
| `spec.apiServerAccess.authorizedNetworks` | placeholder | NKE-DS9-014 |
| `spec.acceleratedRuntime{name,version}` | placeholder | UC-018, NKE-DS9-011 (P1 via API) |
| `status.kubernetesVersion` | live | `ControlPlaneStatus.kubernetesVersion` |
| `status.endpoint` | live | the control-plane VIP |
| `status.certificateAuthorityData` | live | UC-014; the EKS/GKE shape, credential from OIDC |
| `status.oidc{issuerURL,clientID}` | placeholder | UC-014, NKE-DS9-003 |
| `status.release` | placeholder | with `spec.release` |
| `status.acceleratedRuntime` | placeholder | with `spec.acceleratedRuntime` |
| conditions `Ready`, `Progressing` | live | NKE cluster conditions |
| condition `ReleaseCurrent` | placeholder | UC-016 |
| conditions `Validated`, `NetworkValidated`, `AcceleratorValidated` | placeholder | NKE-DS9-012 |

### NodePool

| Field | Status | Basis |
|---|---|---|
| `spec.clusterName` | live, immutable | `clusterID` |
| `spec.instanceType` | live, immutable | `instanceTypeID` |
| `spec.machineImage.name` | live | `osProfile` |
| `spec.machineImage.version` | placeholder | UC-017 (P1) |
| `spec.replicas` | live; `/scale` | `desiredNodeCount` |
| `spec.kubernetesVersion` | placeholder | UC-016 (P1 skew). NKE has a per-pool version; here it arrives with the Release |
| `spec.template.metadata.labels` | live | `nodeLabels` |
| `spec.rollout.strategy.rollingUpdate.maxSurge` | live | `maxSurge`, `recycleBudget` |
| `…rollingUpdate.maxUnavailable` | placeholder | UC-016, UC-017 |
| `spec.repairAcceptance` | placeholder | UC-010 |
| `status.replicas`, `readyReplicas`, `upToDateReplicas` | live | node conditions, `currentNodeSetID` |
| `status.kubernetesVersion`, `status.machineImage` | live | observed |
| conditions `Ready`, `AtDesiredSize`, `RollingOut` | live | |
| conditions `VersionSkewWithinTolerance`, `RepairsPending`, validation | placeholder | UC-016, UC-010, NKE-DS9-012 |

### NKENode

| Field | Status | Basis |
|---|---|---|
| `spec.retire{reason}` | live, immutable once set | `DELETE /nodes/{id}` + `machineHealthIssue.summary` |
| `spec.acceptRepair` | placeholder | UC-010; names `status.repairRequest.id` |
| `status.clusterName`, `status.nodePoolName` | live, selectable | |
| `status.repairRequest{…}` | placeholder | UC-010; operator-written |
| conditions `Ready`, `Healthy`, `Retiring` | live | `kubernetesReady`, `nodeAgentReady`, `deleting` |
| condition `RepairRequested`, validation | placeholder | UC-010, NKE-DS9-012 |

`doc.go` records how each live field is actuated against NKE today, and lists the NKE inputs that
are deliberately not fields (CNI, etcd encryption, control-plane sizing, provider ids) with the
reason each belongs to the release, a mandate, the operator plane or the provider.

## Who touches which view

`rw` is read and write, `r` read only, `–` not present in that role's projection. Controllers
have `rw` on everything and are omitted. Placeholders are marked (ph).

| Field | Tenant | Operator |
|---|---|---|
| `metadata.name`, `namespace`, `labels`, `annotations` | rw | r |
| platform annotations (`internal.nke.nvidia.com/*`) | – | r |
| **NKECluster** `spec.networking` | rw, create-only | r |
| `spec.release`, `spec.apiServerAccess`, `spec.acceleratedRuntime` (ph) | rw | r |
| `status.kubernetesVersion`, `endpoint`, `certificateAuthorityData`, `conditions` | r | r |
| `status.oidc`, `status.release`, `status.acceleratedRuntime` (ph) | r | r |
| **NodePool** `spec.*` (all live fields) | rw | r |
| `spec.kubernetesVersion`, `spec.repairAcceptance` (ph) | rw | r |
| `status.replicas`, `readyReplicas`, `upToDateReplicas`, `kubernetesVersion`, `machineImage`, `conditions` | r | r |
| **NKENode** `spec.retire` | rw, immutable once set | r |
| `spec.acceptRepair` (ph) | rw | r |
| `status.clusterName`, `nodePoolName`, `conditions` | r | r |
| `status.repairRequest` (ph) | r | r; raised outside this kind |
| `status.internal.replaces`, `replacedBy` | – | r |

Two rules hold across the table: every tenant-owned spec field is operator read-only, and no
field is writable by both roles. Access is enforced twice: a field absent from a role's projection
is refused by strict decoding with a 400, and a field present but read-only for that role is
refused by admission when a write from that role would change it.

A hidden field (`status.internal.*` or unmarked) exists only when the controller cannot recompute
it from spec, status, owner references and one read of the backend. Which cluster a pool is bound
to is the owner reference the controller sets, uid included, so a recreated cluster under the same
name is never adopted; which revision the nodes are at is read each reconcile and reported as
`upToDateReplicas`. The NKENode replacement chain is the only hidden state left.

Operators acting on a tenant's behalf (the SRD's "admin level access") impersonate the tenant
through the tenant API, so the write is validated by tenant rules and audited as
operator-on-behalf-of-tenant. Operator intent in its own right (repair requests, validation
re-runs, control-plane operations) is outside these kinds; see "Operator intent" in `doc.go`.

## On the wire

The tenant surface is plain Kubernetes REST: standard paths, standard verbs, JSON bodies, a bearer
token from the operator's OIDC issuer, and the org's namespace in the URL. Anything that speaks to
a kube-apiserver, `kubectl`, client-go, `curl`, works unchanged.

A write body carries intent and a name, nothing else. The URL supplies the group, version, kind and
namespace, so `apiVersion`, `kind` and `metadata.namespace` may be omitted (a body naming a
*different* one is a 400). `metadata.name` is required on `POST`, where the URL has no name, and
optional on `PUT`/`PATCH`. `PATCH` with `Content-Type: application/json` is a merge patch. The
examples below use the dev endpoint `https://127.0.0.1:8443` and `$TOKEN` for the tenant's JWT.

```sh
H=(-H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json")
API=https://127.0.0.1:8443/apis/nke/v1
```

**Discovery.**

```sh
curl -sk "${H[@]}" $API                              # APIResourceList: the kinds, verbs, short names
```

**Create a cluster.** Only `spec` is writable; `status` in a body is a 400. `networking` is
create-only. There is no version field: the cluster runs the release the operator has current
until `spec.release` lands.

```sh
curl -sk "${H[@]}" -X POST $API/namespaces/org-demo/nkeclusters -d @- <<'EOF'
{
  "metadata": {"name": "demo"},
  "spec": {
    "networking": {"podCIDR": "192.168.0.0/16", "serviceCIDR": "10.96.0.0/12"}
  }
}
EOF
```

**Read it back, and build a kubeconfig from it.** The response carries the nine-field
`metadata`, `spec` as written, and the controller's `status`; nothing else exists on the wire.

```sh
curl -sk "${H[@]}" $API/namespaces/org-demo/nkeclusters/demo | jq '{
  version:  .status.kubernetesVersion,
  endpoint: .status.endpoint,
  ca:       .status.certificateAuthorityData,
  ready:    (.status.conditions[] | select(.type=="Ready"))
}'
```

```json
{
  "version": "v1.34.1",
  "endpoint": "https://demo.org-demo.nke.example:6443",
  "ca": "LS0tLS1CRUdJTi…",
  "ready": {"type": "Ready", "status": "True", "reason": "Serving", "lastTransitionTime": "2026-09-10T08:01:00Z"}
}
```

`endpoint` and `certificateAuthorityData` are the cluster half of a kubeconfig; the user half is
an OIDC token from the operator's issuer (UC-014). This API never mints a credential.

**Create a node pool.** `replicas` is the count; `/scale` changes it. The `template` follows
`PodTemplate` naming and carries labels only.

```sh
curl -sk "${H[@]}" -X POST $API/namespaces/org-demo/nodepools -d @- <<'EOF'
{
  "metadata": {"name": "gpu-a"},
  "spec": {
    "clusterName": "demo",
    "instanceType": "gb200-nvl72",
    "machineImage": {"name": "gb200-ubuntu-24.04"},
    "replicas": 2,
    "template": {"metadata": {"labels": {"nvidia.com/gpu.product": "gb200"}}},
    "rollout": {"strategy": {"type": "RollingUpdate", "rollingUpdate": {"maxSurge": 1}}}
  }
}
EOF

# scale
curl -sk "${H[@]}" -X PATCH $API/namespaces/org-demo/nodepools/gpu-a/scale -d '{"spec":{"replicas":4}}'
```

**Nodes.** `NKENode` objects are controller-created; tenants list them (the two `status` fields
are server-side selectable) and write exactly one thing today: `retire`.

```sh
curl -sk "${H[@]}" "$API/namespaces/org-demo/nkenodes?fieldSelector=status.nodePoolName%3Dgpu-a" \
  | jq '.items[] | {node: .metadata.name, conditions: [.status.conditions[] | {type, status, reason}]}'

# return a node (immutable once set; the pool provisions a replacement)
curl -sk "${H[@]}" -X PATCH $API/namespaces/org-demo/nkenodes/gpu-a-0004 \
  -d '{"spec":{"retire":{"reason":"GPU 3 Xid 79 recurring"}}}'
```

**Refusals are Kubernetes `Status` objects**, so every client already knows how to render them.
An immutable field changed after creation looks like this (message text illustrative):

```json
{
  "kind": "Status", "apiVersion": "v1", "status": "Failure",
  "code": 422, "reason": "Invalid",
  "message": "NodePool.nke \"gpu-a\" is invalid: spec.instanceType: Invalid value: \"gb300-nvl72\": instanceType is immutable",
  "details": {"name": "gpu-a", "group": "nke", "kind": "NodePool",
              "causes": [{"reason": "FieldValueInvalid", "field": "spec.instanceType",
                          "message": "instanceType is immutable"}]}
}
```

Unknown or hidden fields in a body (`status`, `metadata.finalizers`, `status.internal`, any
placeholder) are a 400 from the strict decoder, not silently dropped. Patches are merge
(`application/json` or `application/merge-patch+json`) or JSON patch (`application/json-patch+json`);
server-side apply and strategic merge are 415.

Responses are full Kubernetes objects, with `apiVersion`, `kind`, the nine-field `metadata`,
`spec`, `status`, and `List` envelopes with `items`, because `kubectl`, client-go, informers and
Terraform's Kubernetes provider decode by them. The request side is lenient; the response side is
exact.

## Reading order

1. [`nke/doc.go`](nke/doc.go): the three views, the placeholder rule, and how each live field is
   actuated against NKE today.
2. [`nke/cluster.go`](nke/cluster.go), then [`nke/nodepool.go`](nke/nodepool.go), then
   [`nke/nkenode.go`](nke/nkenode.go): the three kinds. Placeholders sit where the field would go.
3. [`nke/validation.go`](nke/validation.go): readiness validation, entirely a placeholder.

Markers are the spec. `+kubebuilder:validation:*` and `XValidation` rules are what admission
enforces; `+kubebuilder:printcolumn` is what `kubectl get` shows; `+kubebuilder:default` is what an
omitted field becomes. Read them as part of the field.

## Not in this branch

The Release, ReleaseChannel and MachineImage catalog kinds, the Org kind, the generator and its
output, the API server, and the controllers. Nothing here runs; `go build ./internal/apis/...`
type-checks the structs, and that is the whole build story.

## Where the reasoning lives

The fuller rationale, with the SRD requirement and CSP precedent behind each field and the
decision record that fixed this shape, lives on `feat/bread` with the rest of the PoC. It is
deliberately not here: this branch is the shape, and the shape should stand on the types alone.
Ask if a "why" is not answered by a field's own comment.
