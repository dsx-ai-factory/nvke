# Provisioned Cluster API infrastructure provider

This provider binds a Cluster API machine to a host that already exists. The
host is on, the operating system is installed, and the network already gave the
host an address. The provider creates no infrastructure: it records which
machine holds which host, and it publishes the address of that host.

Use it to bootstrap a cluster on hardware that some other process prepared.

## Layout

| Path | Content |
|---|---|
| `api/v1alpha1/` | The four kinds, their deepcopy functions, and the provider ID format |
| `controllers/` | The cluster and machine reconcilers |
| `cmd/manager/` | The standalone controller-manager binary |
| `config/` | CRDs, RBAC, and the controller Deployment |
| `Dockerfile` | The standalone provider image |

The provider imports nothing from the cloud-workflow-engine. It reads only the
Kubernetes API, Cluster API, and its own types. It has its own Go module, binary,
image, and deployment.

## Kinds

| Kind | Scope | Purpose |
|---|---|---|
| `ProvisionedCluster` | Namespaced | Holds the host inventory of one cluster |
| `ProvisionedMachine` | Namespaced | One machine, bound to one host of that inventory |
| `ProvisionedMachineTemplate` | Namespaced | The template that Cluster API renders into a machine |
| `ProvisionedHostClaim` | **Cluster** | Locks one physical host across every cluster |

## How a host is allocated

Each inventory entry is one explicit hostname and IP-address pair:

```yaml
spec:
  hosts:
    - hostname: host-01.example.internal
      ipAddress: 10.0.0.11
      tpmEkPubHash: 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
```

`tpmEkPubHash` is optional. When present, the provider annotates the owning
CAPI Machine with that host's TPM SPIRE-agent identity. CWE uses the annotation
to create the mTLS workload identity for `nvke-agent`.

The binding lives in `ProvisionedMachine.spec.providerID`, in the form
`provisioned://<cluster-name>/<canonical-ip-address>`. The cluster component
keeps private addresses unique within their cluster. There is no allocation
table.

```
1. Wait for the owner Machine. It identifies the owning cluster.
2. If spec.providerID is set, refresh the status from the inventory and stop.
3. List every ProvisionedMachine of the cluster. Read the IP address out of
   each provider ID. That is the set of used hosts.
4. Take the first free hostname/IP pair whose labels match spec.hostSelector.
5. Write spec.providerID with optimistic concurrency. On a conflict, repeat
   from step 3 against a fresh view. This enforces that no two machines or clusters can represent the same provisioned host.
6. Write status.hostName, status.addresses and
   status.initialization.provisioned.
```

## How the Node gets its provider ID

After the machine controller claims a host, it reads the standard CAPI
`<cluster-name>-kubeconfig` Secret from the cluster namespace. It uses that
kubeconfig to find the same-named Node in the tenant cluster and sets
`Node.spec.providerID`.

The machine controller does not overwrite a different provider ID. It reports
that conflict as an error. It waits and tries again when the kubeconfig Secret
or the tenant Node does not exist yet.

## Lifecycle limitation

The provider allows deletion of a `ProvisionedMachine` that has not claimed a
host. After the machine claims a host, individual deletion is not supported.
The provider cannot reset or power off the host, so deletion could leave a
running node behind and then assign the same host again. The controller keeps a
finalizer on the object and reports `DeletionNotSupported` instead.

Do not use machine rollouts, scale-down, or remediation with this bootstrap-only
provider. Tear down the complete Kind management cluster after bootstrap. This
removes the machines, claims, and provider together without reusing a live host.

## Install

Build the image and render the clusterctl artifacts from the repository root:

```bash
make -C cluster-api-provider-provisioned docker-build \
  IMG=registry.example/cluster-api-provider-provisioned:v0.1.0
CONTROLLER_IMG=registry.example/cluster-api-provider-provisioned:v0.1.0 \
  cluster-api-provider-provisioned/hack/release-manifests.sh
```

For a source-tree installation:

```bash
kubectl apply -k cluster-api-provider-provisioned/config/default
```

The generated `cluster-api-provider-provisioned/out/infrastructure-components.yaml`
and `metadata.yaml` are the artifacts for a clusterctl provider repository. CWE
still needs `capi.enabled=true` for its provider-independent CAPI and bootstrap
controllers. Supporting clusterctl install will be added in a subsequent commit.

## Regenerating

The module pins `controller-gen`. Generate the deepcopy implementations, CRDs,
and controller RBAC with:

```bash
make -C cluster-api-provider-provisioned generate manifests
```

Commit the generated files. `generate-check` fails when they are stale.
