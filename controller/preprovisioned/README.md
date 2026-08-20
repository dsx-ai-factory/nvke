# Preprovisioned Cluster API infrastructure provider

This provider binds a Cluster API machine to a host that already exists. The
host is on, the operating system is installed, and the network already gave the
host an address. The provider creates no infrastructure: it records which
machine holds which host, and it publishes the address of that host.

Use it to bootstrap a cluster on hardware that some other process prepared.

Design and decisions: [../docs/preprovisioned-capi-bootstrap.md](../docs/preprovisioned-capi-bootstrap.md).

## Layout

| Path | Content |
|---|---|
| `api/v1alpha1/` | The three kinds, their deepcopy functions, and the provider ID format |
| `controllers/` | The cluster reconciler and the machine reconciler |
| `config/crd/` | The CRDs to apply before the controllers start |

The package imports nothing from the cloud-workflow-engine. It reads only the
Kubernetes API, Cluster API, and its own types. To move the provider to its own
module or repository, move this directory and add a `go.mod`; the only change on
the CWE side is the two `SetupWithManager` calls in `cmd/worker/main.go`.

## Kinds

| Kind | Scope | Purpose |
|---|---|---|
| `PreprovisionedCluster` | Namespaced | Holds the host inventory of one cluster |
| `PreprovisionedMachine` | Namespaced | One machine, bound to one host of that inventory |
| `PreprovisionedMachineTemplate` | Namespaced | The template that Cluster API renders into a machine |
| `PreprovisionedHostClaim` | **Cluster** | Locks one physical host across every cluster |

## How a host is allocated

The binding lives in `PreprovisionedMachine.spec.providerID`, in the form
`preprovisioned://<cluster-name>/<host-name>`. There is no allocation table.

```
1. Wait for the owner Machine. Its labels give the role of the machine.
2. If spec.providerID is set, refresh the status from the inventory and stop.
3. List every PreprovisionedMachine of the cluster. Read the host name out of
   each provider ID. That is the set of used hosts.
4. Take the first host that is free, that accepts the role, and whose labels
   match spec.hostSelector.
5. Write spec.providerID with optimistic concurrency. On a conflict, repeat
   from step 3 against a fresh view.
6. Write status.hostName, status.addresses and
   status.initialization.provisioned.
```

Three properties follow:

- **The binding survives a restart.** The provider ID is the record, so the
  provider needs no cache and no lease. A restart between step 5 and step 6 is
  repaired by step 2 on the next reconcile.
- **Two machines cannot share a host.** Step 5 writes with optimistic
  concurrency, so the API server accepts only the first of two writers.

- **Two clusters cannot share a machine.** Steps 4 and 5 also create a
  cluster-scoped `PreprovisionedHostClaim` per identity of the host — its
  `machineID` if given, and each address. Each cluster has its own inventory in
  its own namespace, so this is the only scope at which the overlap is visible.
  A host locked by another cluster is skipped and the next candidate tried;
  `HostsClaimedElsewhere` is reported only when every candidate is taken.

The provider holds no finalizer. Three invariants keep the claims correct
instead: a machine holding no host holds no claims, a machine that is gone
leaves none, and a claim whose holder no longer exists is reclaimed by the next
claimant rather than stranding the host. So a wedged provider can never block a
machine from being deleted.

## Install

```bash
kubectl apply -f config/crd/
```

Then set `preprovisioned.enabled=true` and `capi.enabled=true` on the CWE chart.
The controllers watch the kinds above, so the CRDs must exist first.

## Try it

`../hack/create-preprovisioned-cluster.sh` creates a cluster against a local CWE.
Give it the inventory:

```bash
PREPROVISIONED_HOSTS="host-01=10.0.0.11,host-02=10.0.0.12,host-03=10.0.0.13" \
  ../hack/create-preprovisioned-cluster.sh
```

## Regenerating

The repository carries no controller-gen, so `zz_generated.deepcopy.go` and the
files under `config/crd/` are written by hand. Every type carries the matching
kubebuilder markers, so controller-gen can take both over later without a change
to the types.
