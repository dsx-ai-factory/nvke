# The NKE API

This directory is the API of NKE: the kinds a tenant sees through `kubectl`, the kinds an operator
publishes, and the shape the management cluster stores. Everything else in NKE — the API server,
the controllers, the code generator — is downstream of the nine hand-written files here.

This PR contains those nine files and the annotated examples under [`examples/`](../../examples/).
It contains no generated code and no behaviour. Nothing here is served directly; what serves it
arrives in later PRs (see the end).

## One hand-written package, three served shapes

```
internal/apis/nke/           the hub: one Go struct per kind, EVERY field, +nke:external marking the tenant-visible ones
        │
        │  cmd/nke-gen (next PR)
        ├──► nke/v1  <Kind>            public shape: only +nke:external fields, nine-field ObjectMeta   ← what tenants see
        └──► nke/v1  <Kind>Internal    full shape: every field, real metav1.ObjectMeta                   ← what is stored
                        │
                        │  controller-gen crd (next PR)
                        └──► internal.nke.nvidia.com/v1  CRDs on the management cluster, same resource names (nkeclusters, nodepools, …)
```

The hub is the single source of truth. A field exists on the tenant surface if and only if it
carries `+nke:external`; an unmarked field is controller-only and hidden. The two served shapes are
projections of the same struct, so there is one place to review and one place to change.

`internal/apis/nke/management/v1/` mirrors the hub under the CRD group: the `<Kind>Internal` types
are field-for-field the hub types, defined as separate structs only because `controller-gen crd`
needs a declaration. `Org` lives there too; it is never tenant-facing and has no hub/external split.
This PR includes `org.go` from that package and nothing else of it.

## The allowlist rule, once

Hub (`cluster.go`, trimmed):

```go
type NKEClusterStatus struct {
	// KubernetesVersion is the version the control plane is running.
	// +nke:external
	KubernetesVersion string `json:"kubernetesVersion,omitempty"`

	// Conditions are Ready, Progressing, ReleaseCurrent and the validation conditions.
	// +nke:external
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Internal is controller-only state the tenant never sees.
	Internal NKEClusterInternalState `json:"internal,omitempty"`
}
```

What `nke-gen` emits into `nke/v1` for tenants — the marked fields, the doc comments, and nothing
else:

```go
type NKEClusterStatus struct {
	// KubernetesVersion is the version the control plane is running.
	KubernetesVersion string `json:"kubernetesVersion,omitempty"`

	// Conditions are Ready, Progressing, ReleaseCurrent and the validation conditions.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
```

`status.internal` is absent from the public type, not masked out of it: a tenant body carrying it
fails strict decoding with a 400, and a `kubectl get` can never return it. The same rule gives the
public kinds a generated nine-field `ObjectMeta` (`name`, `namespace`, `uid`, `resourceVersion`,
`generation`, `creationTimestamp`, `deletionTimestamp`, `labels`, `annotations`): finalizers,
ownerReferences and managedFields do not exist on the tenant surface. That excerpt is the only
generated code in this PR; it is an illustration, not a file.

There is no `spec.internal`. Operator intent that is not tenant-facing goes on the stored object
through the operator's own access (the `nkenodes/status` repair request, the
`internal.nke.nvidia.com/paused` and `operation` annotations), never into a hidden slice of spec.

## Who touches which shape

| Actor | Reads / writes | Through |
|---|---|---|
| Tenant (`kubectl`, `org-<id>` namespace) | `nke/v1` public kinds: `NKECluster`, `NodePool`, `NKENode`; reads `Release`, `ReleaseChannel` | `nke-apiserver` |
| `nke-apiserver` | decodes tenant bodies strictly as the public shape, converts to the hub, stores as `internal.nke.nvidia.com/v1`; projects the reverse on read | — |
| Controllers, machine-provider adapter | `internal.nke.nvidia.com/v1` whole, including `status.internal` | the management cluster directly |
| Operator | `internal.nke.nvidia.com/v1` whole; publishes `Release`/`ReleaseChannel`, writes `Org`, writes repair requests and annotations | the management cluster directly |

## On the wire

The tenant surface is plain Kubernetes REST: standard paths, standard verbs, JSON bodies, a bearer
token from the operator's OIDC issuer, and the org's namespace in the URL. Anything that speaks to
a kube-apiserver — `kubectl`, client-go, `curl` — works unchanged.

A write body carries intent and a name, nothing else. The URL supplies the group, version, kind and
namespace, so `apiVersion`, `kind` and `metadata.namespace` may be omitted (a body naming a
*different* one is a 400). `metadata.name` is required on `POST`, where the URL has no name, and
optional on `PUT`/`PATCH`. `PATCH` with `Content-Type: application/json` is a merge patch. The
examples below use the dev endpoint `https://127.0.0.1:8443` and `$TOKEN` for the tenant's JWT.

```sh
H=(-H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json")
API=https://127.0.0.1:8443/apis/nke/v1
```

**Discovery and the catalog.** The group is discoverable; the operator's catalog is readable by
every tenant, hidden entries omitted from `list` but still fetchable by name.

```sh
curl -sk "${H[@]}" $API                              # APIResourceList: the kinds, verbs, short names
curl -sk "${H[@]}" $API/releasechannels/stable | jq .spec.releaseName
curl -sk "${H[@]}" $API/releases | jq '.items[] | {name: .metadata.name, k8s: .spec.kubernetes.version, deprecated: .spec.deprecated}'
```

**Create a cluster.** Only `spec` is writable; `status` in a body is a 400. `networking` is
create-only.

```sh
curl -sk "${H[@]}" -X POST $API/namespaces/org-demo/nkeclusters -d @- <<'EOF'
{
  "metadata": {"name": "demo"},
  "spec": {
    "release": {"channel": "stable"},
    "networking": {"podCIDR": "192.168.0.0/16", "serviceCIDR": "10.96.0.0/12"}
  }
}
EOF
```

**Read it back.** The response carries the nine-field `metadata`, `spec` as written, and the
controller's `status`; nothing else exists on the wire.

```sh
curl -sk "${H[@]}" $API/namespaces/org-demo/nkeclusters/demo | jq '{
  version:   .status.kubernetesVersion,
  release:   .status.release,
  endpoint:  .status.endpoint,
  ready:     (.status.conditions[] | select(.type=="Ready")),
  validated: (.status.conditions[] | select(.type=="Validated"))
}'
```

```json
{
  "version": "v1.34.1",
  "release": {
    "channel": "stable",
    "current": "v1.34.1-cr2.1.0",
    "offered": "v1.34.2-cr2.1.0",
    "available": ["v1.34.2-cr2.1.0", "v1.34.1-cr2.1.0"]
  },
  "endpoint": "https://demo.org-demo.nke.example:6443",
  "ready":     {"type": "Ready",     "status": "True", "reason": "Serving", "lastTransitionTime": "2026-09-10T08:01:00Z"},
  "validated": {"type": "Validated", "status": "True", "reason": "Passed",  "lastTransitionTime": "2026-09-10T08:09:52Z"}
}
```

**Hold or upgrade** by selecting a Release within the channel (`spec.release.name` must be in
`status.release.available`, else 422). A merge patch is the natural verb for one field.

```sh
curl -sk "${H[@]}" -X PATCH $API/namespaces/org-demo/nkeclusters/demo \
  -d '{"spec":{"release":{"name":"v1.34.2-cr2.1.0"}}}'
```

**Create a node pool.** `replicas` xor `autoscaling`; the `template` follows `PodTemplate` naming.

```sh
curl -sk "${H[@]}" -X POST $API/namespaces/org-demo/nodepools -d @- <<'EOF'
{
  "metadata": {"name": "gpu-a"},
  "spec": {
    "clusterName": "demo",
    "instanceType": "gb200-nvl72",
    "autoscaling": {"minReplicas": 2, "maxReplicas": 4},
    "template": {
      "metadata": {"labels": {"nvidia.com/gpu.product": "gb200"}},
      "spec": {
        "taints": [{"key": "nvidia.com/gpu", "value": "present", "effect": "NoSchedule"}],
        "kubelet": {"maxPods": 110}
      }
    },
    "rollout": {"strategy": {"type": "RollingUpdate", "rollingUpdate": {"maxSurge": 1, "maxUnavailable": 0}}},
    "repairAcceptance": "Manual"
  }
}
EOF
```

**Nodes and break-fix.** `NKENode` objects are controller-created; tenants list them (the two
`status` fields are server-side selectable), and write exactly two things: `retire` and
`acceptRepair`.

```sh
curl -sk "${H[@]}" "$API/namespaces/org-demo/nkenodes?fieldSelector=status.nodePoolName%3Dgpu-a" \
  | jq '.items[] | {node: .metadata.name, repair: .status.repairRequest, conditions: [.status.conditions[] | select(.type=="Healthy" or .type=="RepairRequested") | {type, status, reason}]}'

# accept the operator's open repair request on one node
curl -sk "${H[@]}" -X PATCH $API/namespaces/org-demo/nkenodes/gpu-a-0003 \
  -d '{"spec":{"acceptRepair":true}}'

# return a node yourself (immutable once set; the pool provisions a replacement)
curl -sk "${H[@]}" -X PATCH $API/namespaces/org-demo/nkenodes/gpu-a-0004 \
  -d '{"spec":{"retire":{"reason":"GPU 3 Xid 79 recurring"}}}'
```

**Refusals are Kubernetes `Status` objects**, so every client already knows how to render them.
The `invalid/` examples each produce one; a pin outside `available` looks like this (message text
illustrative — the shape is the point):

```json
{
  "kind": "Status", "apiVersion": "v1", "status": "Failure",
  "code": 422, "reason": "Invalid",
  "message": "NKECluster.nke \"demo\" is invalid: spec.release.name: Invalid value: \"v1.33.0-cr2.0.1\": not in status.release.available [v1.34.2-cr2.1.0 v1.34.1-cr2.1.0]",
  "details": {"name": "demo", "group": "nke", "kind": "NKECluster",
              "causes": [{"reason": "FieldValueInvalid", "field": "spec.release.name",
                          "message": "not in status.release.available [v1.34.2-cr2.1.0 v1.34.1-cr2.1.0]"}]}
}
```

Unknown or hidden fields in a body (`status`, `metadata.finalizers`, `status.internal`) are a
400 from the strict decoder, not silently dropped. Patches are merge (`application/json` or
`application/merge-patch+json`) or JSON patch (`application/json-patch+json`); server-side apply
and strategic merge are 415.

Responses are full Kubernetes objects — `apiVersion`, `kind`, the nine-field `metadata`, `spec`,
`status`, and `List` envelopes with `items` — because `kubectl`, client-go, informers and
Terraform's Kubernetes provider decode by them. The request side is lenient; the response side is
exact.

## Reading order

1. [`nke/doc.go`](nke/doc.go) — the rule above, in two paragraphs.
2. [`nke/cluster.go`](nke/cluster.go) → [`nke/nodepool.go`](nke/nodepool.go) → [`nke/nkenode.go`](nke/nkenode.go) — the tenant kinds.
3. [`nke/release.go`](nke/release.go) → [`nke/releasechannel.go`](nke/releasechannel.go) — the operator catalog.
4. [`nke/clusterrelease.go`](nke/clusterrelease.go) — controller-owned; the per-cluster release position.
5. [`nke/validation.go`](nke/validation.go) — the validation condition names and the hidden
   `ObservedValidation` shape the adapter reports.
6. [`nke/management/v1/org.go`](nke/management/v1/org.go) — the org, its members and their roles.
7. [`examples/`](../../examples/) — every field of every kind in YAML, with the happy path, the
   promotion story, and one refused write per validation rule under `examples/invalid/`.

Markers are the spec. `+kubebuilder:validation:*` and `XValidation` rules are what admission
enforces; `+kubebuilder:printcolumn` is what `kubectl get` shows; `+kubebuilder:default` is what an
omitted field becomes. Read them as part of the field.

## Legend

The examples and the proposal mark each field one of three ways:

- **now** — populated and acted on in the PoC.
- **shape** — in the API, validated and round-tripped, but nothing populates or acts on it yet.
  Tenant kinds only, P0 requirements only. Its doc comment names who will write it. The tenant
  API is frozen at its P0 end state so it does not grow field-by-field as components arrive.
- **later** — recorded in the proposal so it is not designed twice; not in the API, and refused
  by the strict decoder if sent.

The type files themselves carry no delivery status; they read as the contract.

## What is not in this PR

| Next PRs | Contents |
|---|---|
| Codegen | `cmd/nke-gen`, `internal/gen`, `codegen.sh`, the generated `nke/v1` package, `management/v1/types.go`, `manifests/crds/` — mechanical; review the golden fixture |
| API server | `nke-apiserver`: authn, impersonation, strict decoding, projection, admission (`skew.go` and `available.go` from this package ship with it), discovery, OpenAPI, watch |
| Controllers | the reconcilers, `cmd/nke-sim-adapter` (the PoC machine-provider adapter), RBAC manifests, `verify.sh` |

Nothing in this PR runs. `go build ./internal/apis/...` type-checks the structs; that is the whole
build story here.

## Where the reasoning lives

The type comments and example headers cite two documents by section: `docs/PROPOSAL-API-FIELDS.md`
(the per-field rationale, with the SRD requirement and CSP precedent for each field) and
`docs/DECISIONS.md` (the decision record; §32, §33, §35, §36, §38, §39 and §49–§59 fix this
shape). Both live on `feat/bread` with the rest of NKE and are deliberately not in this PR: this
one is the shape, and the shape should stand on the types and the examples alone. Ask for either
if a "why" is not answered by a field's own comment.
