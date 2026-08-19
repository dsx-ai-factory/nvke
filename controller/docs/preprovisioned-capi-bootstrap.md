# Proposal: cluster bootstrapping on preprovisioned hosts

> Status: **proposal** — for alignment. Describes how CWE bootstraps a cluster
> on hosts that already exist, using generic-CAPI mode and a new infrastructure
> provider, with no change to the REST API.

| | |
|---|---|
| Ticket | [NKX-14875](https://jirasw.nvidia.com/browse/NKX-14875) |
| Components | `controller/` (CWE), new infrastructure provider |
| Supersedes | The approach in `nke/cloud-workflow-engine!996`, which changed the REST API |

## 1. Summary

CWE must bootstrap a Kubernetes cluster on hosts that already exist. The hosts
are on, the operating system is installed, and the network gives each host an IP
address. No component must create infrastructure.

This proposal adds no field to the CWE REST API and no column to the database.
The caller sends the host inventory inside the `capiInfraCluster` manifest that
the API already accepts. A new, small Cluster API infrastructure provider reads
that inventory. For each machine, the provider selects a host that no other
machine uses, and reports the address of that host through the Cluster API
contract. The rest of the CWE generic-CAPI flow does not change.

The provider creates nothing. It records which host belongs to which machine,
and it publishes the address that the network already assigned.

## 2. Goals and non-goals

### Goals

- Bootstrap a cluster on preprovisioned hosts.
- Make no change to the CWE REST API, to the OpenAPI schema, or to the database
  schema.
- Carry the inventory (host names and IP addresses) in the infrastructure
  cluster manifest.
- Keep the Nico/Forge path unchanged.
- Keep the change small enough to run during a bootstrap, when few components
  are available.

### Non-goals

- Power control, BMC access, PXE boot, or operating-system installation.
- IP address assignment on the network. The network assigns the address before
  CWE starts. The provider only records which host is in use.
- A replacement for the Nico provider. This provider is for hosts that already
  exist.
- Multi-tenancy or quota control on the inventory.

## 3. Why the REST API does not change

CWE has a generic-CAPI mode. Appendix C lists the fields that this mode uses. A
cluster enters this mode when `siteID` is the nil UUID. The API then requires
the caller to send provider manifests, and it rejects all Forge-specific fields.

The table shows that the existing fields already carry every input this design
needs.

| Input | Existing field | Level |
|---|---|---|
| Provider selection | `siteID` = nil UUID | Cluster |
| Host inventory | `capiInfraCluster` (free-form manifest) | Cluster |
| Machine shape | `capiInfraMachineTemplate` (free-form manifest) | Node pool, control plane config |
| Control plane VIP | `controlPlaneVIP` | Cluster |
| Kubernetes version | `controlPlaneKubernetesVersion` | Cluster |
| CNI | `cniPlugin`, `cniVersion` | Cluster |
| Extra files on the host | `capiBootstrapFiles` | Node pool, control plane config |
| Extra commands | `capiPreKubeadmCommands` | Node pool, control plane config |
| Node agent identity | `capiSpireAttestationMethod` | Node pool, control plane config |

The API validates the two manifests for structure only. It requires
`apiVersion` and `kind`, and it requires the API group to be
`infrastructure.cluster.x-k8s.io`. See
`validateCAPIInfrastructureManifest` in
[`controller/internal/server/capi_manifest.go`](../internal/server/capi_manifest.go).
CWE does not read the body of either manifest. A new kind therefore needs no
API change.

**One deployment step is necessary.** The controllers that provision clusters
are site-scoped. They list clusters for each registered site. A generic-CAPI
cluster uses the nil UUID as its site ID, so a site row with the nil UUID must
exist. The development flow already does this with
[`controller/hack/register-default-site.sh`](../hack/register-default-site.sh).
The bootstrap flow must do the same. This is a call to the existing
`POST /api/sites` endpoint, not a schema change.

## 4. Design

### 4.1 New custom resources

Three kinds go into API group `infrastructure.cluster.x-k8s.io`, version
`v1alpha1`. The kinds follow the same shape as the Nico provider kinds, which
Appendix D shows for comparison.

| Kind | Purpose |
|---|---|
| `PreprovisionedCluster` | Holds the host inventory for one cluster |
| `PreprovisionedMachine` | One machine, bound to one host from the inventory |
| `PreprovisionedMachineTemplate` | Template that CWE renders into a `PreprovisionedMachine` |

#### PreprovisionedCluster

```go
// PreprovisionedHost describes one host that already exists on the network.
type PreprovisionedHost struct {
    // Name identifies the host in the inventory. It must be unique in the
    // cluster. It is a label for operators; it does not become the Kubernetes
    // node name.
    // +kubebuilder:validation:MinLength=1
    Name string `json:"name"`

    // Addresses are the addresses that the network already assigned to this
    // host. At least one address must be present. The provider copies these
    // addresses to the machine status.
    // +kubebuilder:validation:MinItems=1
    Addresses []clusterv1.MachineAddress `json:"addresses"`

    // Role limits which machines can use this host. An empty value allows any
    // machine.
    // +kubebuilder:validation:Enum=control-plane;worker
    // +optional
    Role string `json:"role,omitempty"`

    // Labels are copied to the PreprovisionedMachine that uses this host.
    // +optional
    Labels map[string]string `json:"labels,omitempty"`
}

type PreprovisionedClusterSpec struct {
    // Hosts is the inventory. The provider gives one host to each machine.
    // +kubebuilder:validation:MinItems=1
    Hosts []PreprovisionedHost `json:"hosts"`

    // ControlPlaneEndpoint is the endpoint that clients use to reach the API
    // server. Set it when a VIP is in use. See section 6.2.
    // +optional
    ControlPlaneEndpoint clusterv1.APIEndpoint `json:"controlPlaneEndpoint,omitempty,omitzero"`
}

type PreprovisionedClusterStatus struct {
    // Initialization reports the state that the Cluster API contract requires.
    // +optional
    Initialization PreprovisionedClusterInitializationStatus `json:"initialization,omitempty,omitzero"`

    // AllocatedHosts reports which host each machine uses. It is derived from
    // the PreprovisionedMachine objects and exists for operators only. No
    // controller reads it.
    // +optional
    AllocatedHosts []HostAllocation `json:"allocatedHosts,omitempty"`

    // AvailableHosts is the count of hosts that no machine uses.
    // +optional
    AvailableHosts int32 `json:"availableHosts,omitempty"`

    // Conditions describes the current state.
    // +optional
    Conditions []metav1.Condition `json:"conditions,omitempty"`
}
```

#### PreprovisionedMachine

```go
type PreprovisionedMachineSpec struct {
    // ProviderID is the binding between this machine and one host in the
    // inventory. The provider sets it once, in the form
    // preprovisioned://<cluster-name>/<host-name>. The provider never changes
    // it and never clears it.
    // +optional
    // +kubebuilder:validation:MinLength=1
    ProviderID string `json:"providerID,omitempty"`

    // HostSelector limits which hosts this machine can use. The provider
    // matches it against PreprovisionedHost.Labels.
    // +optional
    HostSelector map[string]string `json:"hostSelector,omitempty"`
}

type PreprovisionedMachineStatus struct {
    // Initialization reports the state that the Cluster API contract requires.
    // Provisioned becomes true when the machine holds a host.
    // +optional
    Initialization PreprovisionedMachineInitializationStatus `json:"initialization,omitempty,omitzero"`

    // HostName is the name of the host that this machine holds.
    // +optional
    HostName string `json:"hostName,omitempty"`

    // Addresses are the addresses of that host. CWE reads this field to find
    // the node agent.
    // +optional
    Addresses []clusterv1.MachineAddress `json:"addresses,omitempty"`

    // Conditions describes the current state.
    // +optional
    Conditions []metav1.Condition `json:"conditions,omitempty"`
}
```

`PreprovisionedMachineTemplate` follows the standard template shape:
`spec.template.spec` holds a `PreprovisionedMachineSpec`. Cluster API strips the
`Template` suffix and copies `spec.template.spec` when it renders the object.
CWE calls the upstream helper for this, so no provider code is necessary.

### 4.2 How the provider allocates a host

The binding lives in `PreprovisionedMachine.spec.providerID`. The provider does
not keep a separate allocation table.

```
1. Read the owner Machine. If it is absent, wait.
2. Read the PreprovisionedCluster for the cluster.
3. If spec.providerID is set, the machine already holds a host.
   Refresh status from the inventory entry and stop.
4. List every PreprovisionedMachine in the namespace for the same cluster.
   Collect the host name from each spec.providerID. This is the used set.
5. Select the first host that:
   - is not in the used set,
   - matches spec.hostSelector, and
   - matches the role of the owner Machine.
   If no host matches, set condition Ready=False with reason NoHostAvailable
   and requeue.
6. Write spec.providerID with the selected host name. Use optimistic
   concurrency. On a conflict, requeue and repeat from step 4.
7. Write status.hostName, status.addresses, and
   status.initialization.provisioned=true.
```

Two properties come from this design:

- **The binding survives a restart.** `spec.providerID` is the record. The
  provider needs no cache and no lease. If the provider restarts between step 6
  and step 7, step 3 repairs the status.
- **Two machines cannot take the same host.** Step 6 writes with optimistic
  concurrency. The API server rejects the second write, and the loser repeats
  the search with a fresh view.

The provider must not write the binding into `status`. A lost status update
would release a host that a running node still uses.

### 4.3 What the cluster controller does

The cluster controller does very little:

1. Validate the inventory. Host names must be unique. Every host must have at
   least one address.
2. Copy `spec.controlPlaneEndpoint` to the parent `Cluster` if the parent has
   no endpoint, as the contract expects.
3. Set `status.initialization.provisioned = true`.
4. Recompute `status.allocatedHosts` and `status.availableHosts` for operators.

Step 3 is not optional. The core Cluster API machine controller waits for the
cluster infrastructure to report `provisioned` before it acts on any machine. A
provider that skips this step stops the whole flow.

### 4.4 Where the code goes

**Recommendation: add the controllers to the existing CWE controller-runtime
manager, and put the API types in a separate package.**

CWE already starts a controller-runtime manager in
[`controller/cmd/worker/main.go`](../cmd/worker/main.go). The
manager takes scheme functions as arguments, so a new provider needs one more
argument and one more `SetupWithManager` call. CWE also already has the RBAC it
needs on the management cluster.

The reason is the use case. This provider exists for a bootstrap, when the
smallest number of components must run. One binary and one deployment is a
direct advantage. The provider is also small: it creates no infrastructure and
calls no external API.

CWE then acts as the consumer and as the provider for the same objects. This is
unusual but it is safe. The two roles use different code paths and different
field managers. CWE writes the spec as a consumer, and the provider writes the
status and the provider ID.

Alternatives, with the reason to reject each:

| Option | Reject because |
|---|---|
| Separate repository, like `cluster-api-provider-nico` | A second release train and a second deployment for about 400 lines of code. It also adds a component to the bootstrap, which is the opposite of the goal. |
| Separate Go module in this repository, like [`controller/hack/capd-reboot-controller`](../hack/capd-reboot-controller/) | Correct pattern for a real provider, and a good later step. It still adds a deployment to the bootstrap. |

The API types go in their own package, for example
`controller/api/preprovisioned/v1alpha1`, with no dependency on CWE internals.
A later move to a separate module then only moves files.

### 4.5 What the caller sends

Appendix B gives the full request. The important part is the infrastructure
cluster manifest:

```json
"capiInfraCluster": {
  "apiVersion": "infrastructure.cluster.x-k8s.io/v1alpha1",
  "kind": "PreprovisionedCluster",
  "spec": {
    "controlPlaneEndpoint": { "host": "10.0.0.10", "port": 6443 },
    "hosts": [
      { "name": "host-01", "role": "control-plane",
        "addresses": [{ "type": "InternalIP", "address": "10.0.0.11" }] },
      { "name": "host-02", "role": "control-plane",
        "addresses": [{ "type": "InternalIP", "address": "10.0.0.12" }] },
      { "name": "host-03", "role": "control-plane",
        "addresses": [{ "type": "InternalIP", "address": "10.0.0.13" }] }
    ]
  }
}
```

and the machine template, which is almost empty because there is nothing to
create:

```json
"capiInfraMachineTemplate": {
  "apiVersion": "infrastructure.cluster.x-k8s.io/v1alpha1",
  "kind": "PreprovisionedMachineTemplate",
  "spec": { "template": { "spec": {} } }
}
```

## 5. How the existing flow uses this

The table follows one node from the API call to a joined node. Only the two
rows in bold contain new code.

| Step | Component | Behaviour |
|---|---|---|
| 1 | `POST /api/clusters` | Stores the cluster row and the manifest. Starts `ProvisionCluster`. No change. |
| 2 | `ProvisionCluster` | Applies the namespace, the `PreprovisionedCluster`, and the core `Cluster`. No change. |
| 3 | **Cluster controller** | **Validates the inventory. Reports `provisioned`.** |
| 4 | Node pool and node set controllers | Create pending node rows. No change. |
| 5 | `NodeScheduler` | Starts one `ProvisionNode` per pending node. No change. |
| 6 | `CreateCAPIInfraMachineForNode` | Renders the template into a `PreprovisionedMachine`. Writes the bootstrap secret and the `Machine`. No change. |
| 7 | **Machine controller** | **Selects a free host. Writes the provider ID and the addresses.** |
| 8 | `WaitForCAPIMachineReady` | Reads the address and stores it on the node row. No change. |
| 9 | `NodePreflight` | Contacts the node agent at that address. No change. |
| 10 | `kubeadm init` or `kubeadm join` | Runs through the node agent. No change. |
| 11 | `ProvisionCluster` | Publishes the kubeconfig secret. Installs DNS. Marks the cluster provisioned. No change. |

Step 6 has one consequence to note. CWE writes a bootstrap data secret for every
CAPI machine. On a preprovisioned host, no component reads that secret, because
the host booted before CWE existed. Section 6.1 covers this.

## 6. Decisions to align on

### 6.1 How the node agent gets an identity

This is the largest open point.

CWE reaches each node through the node agent. On the Forge path, cloud-init
installs the agent and its SPIRE material at first boot. A preprovisioned host
already booted, so no cloud-init runs, and the bootstrap secret has no reader.

Three options:

| Option | Description | Assessment |
|---|---|---|
| A. Insecure agent during bootstrap | Run CWE with SPIFFE off. The agent client then uses a plain connection. See `getGRPCConnection` in [`controller/internal/agent/client.go`](../internal/agent/client.go). | Simplest. Acceptable for a bootstrap on a closed network. Not acceptable for a tenant cluster. |
| B. Preinstalled identity | The host image carries the agent and a SPIRE identity from an earlier step. | Correct for production. It moves work to whichever process prepares the hosts. |
| C. Provider-side delivery | The provider writes the bootstrap data to the host over SSH. | Gives the provider a credential and a network path to every host. Rejected: it makes a small provider into a configuration-management tool. |

**Recommendation: allow A for the bootstrap case, and target B.** Keep the
provider out of this question in both cases. This choice needs no CRD field.

### 6.2 Where the control plane VIP goes

The ticket asks for the kube-vip address in the infrastructure cluster manifest.
CWE already has a `controlPlaneVIP` field on the cluster, and CWE, not the
provider, configures kube-vip on each control plane node.

The design above puts `controlPlaneEndpoint` in the `PreprovisionedCluster`
because the Cluster API contract expects the infrastructure cluster to be able
to report it. That value is for Cluster API consumers.

**Recommendation: `controlPlaneVIP` stays the input that CWE acts on.** The
`controlPlaneEndpoint` in the manifest is optional, and it only makes the
Cluster API objects correct for other readers. Setting the same address twice is
acceptable. Making the provider own kube-vip is not.

### 6.3 The tenant node keeps no provider ID

Cluster API links a `Machine` to a `Node` by provider ID. On the Nico path,
`NicoProviderIDController` copies the provider ID to the tenant node. On the
CAPD path, CAPD does it. Nothing would do it for preprovisioned hosts.

CWE does not depend on the link, so the cluster still comes up. But the
`Machine` objects never reach the `Running` phase, `status.nodeRef` stays empty,
and machine health checks cannot work.

**Recommendation: generalise the existing controller.** The logic in
[`controller/internal/controllers/capi/nico_provider_id_controller.go`](../internal/controllers/capi/nico_provider_id_controller.go)
is already generic, except that it watches a `NicoMachine` and reads
Forge-specific labels from its status. A version that watches `Machine` and
reads `spec.providerID` from the infrastructure reference serves both providers.
The topology labels stay behind a type check.

### 6.4 Host name against node name

CWE generates the node name and passes it to kubeadm as
`nodeRegistration.name`. The Kubernetes node name is therefore the CWE name, not
the hostname of the preprovisioned host. `PreprovisionedHost.Name` is only an
inventory label.

**Recommendation: accept this.** It keeps naming consistent between providers.
Operators find the mapping in `PreprovisionedMachine.status.hostName` and in the
provider ID.

### 6.5 Who maintains the inventory

Adding a host means a `PATCH` on the cluster with a longer `hosts` list. The
current `PATCH /api/clusters/{id}` handling of `capiInfraCluster` must be
confirmed before this is promised to users.

**Recommendation: for the first version, treat the inventory as fixed at create
time.** Size it for the target cluster. Track growth separately.

## 7. Risks

| Risk | Effect | Control |
|---|---|---|
| An address in the inventory is wrong or stale | The node agent never answers. The node stops at preflight. | The provider validates the format only. The failure is visible on the node condition. |
| An operator lists one host twice under two names | Two machines target the same host. `kubeadm` fails on the second. | The cluster controller rejects duplicate names. It cannot detect duplicate addresses across names; add a second check. |
| The inventory runs out | New nodes stay pending. | Condition `Ready=False`, reason `NoHostAvailable`, and `status.availableHosts`. |
| CWE acts as consumer and provider | A change to one role can affect the other. | Separate packages, separate field managers, and no shared state except the API server. |
| The provider has no fallback | A stuck machine cannot move to another host. | Accepted. `provisioningFallbackFor` returns nil for template-backed nodes today. Deleting the machine releases the host. |

## 8. Work items

| # | Item | Size |
|---|---|---|
| 1 | API types, deepcopy, and CRD manifests | S |
| 2 | Cluster controller: validation, `provisioned`, status | S |
| 3 | Machine controller: selection, provider ID, addresses | M |
| 4 | Register the scheme and the controllers in `cmd/worker` | XS |
| 5 | RBAC and CRD installation in `controller/deploy` | S |
| 6 | Generalise the provider ID controller (section 6.3) | M |
| 7 | A `hack/create-preprovisioned-cluster.sh` script, like the CAPD one | XS |
| 8 | End-to-end test with containers or VMs as preprovisioned hosts | M |

## Appendix A — Sequence

```
Operator            CWE API        Temporal          Provider         Host
   |                   |              |                  |              |
   |-- POST /sites --->|              |                  |              |
   |   (nil UUID)      |              |                  |              |
   |                   |              |                  |              |
   |-- POST /clusters->|              |                  |              |
   |   capiInfraCluster|              |                  |              |
   |   with hosts[]    |              |                  |              |
   |                   |-- outbox --->|                  |              |
   |<-- 201 -----------|              |                  |              |
   |                   |              |                  |              |
   |                   |   ProvisionCluster              |              |
   |                   |              |-- apply -------->|              |
   |                   |              |  Namespace,      |              |
   |                   |              |  Preprovisioned- |              |
   |                   |              |  Cluster,        |              |
   |                   |              |  Cluster         |              |
   |                   |              |                  |-- validate   |
   |                   |              |                  |   inventory  |
   |                   |              |                  |-- provisioned|
   |                   |              |                  |   = true     |
   |                   |              |                  |              |
   |                   |   ProvisionNode (one per node)  |              |
   |                   |              |-- apply -------->|              |
   |                   |              |  Preprovisioned- |              |
   |                   |              |  Machine,        |              |
   |                   |              |  bootstrap secret|              |
   |                   |              |  Machine         |              |
   |                   |              |                  |-- select     |
   |                   |              |                  |   free host  |
   |                   |              |                  |-- providerID |
   |                   |              |                  |-- addresses  |
   |                   |              |<-- address ------|              |
   |                   |              |                  |              |
   |                   |              |-- node agent preflight -------->|
   |                   |              |-- kubeadm init / join --------->|
   |                   |              |                  |              |
   |                   |<-- provisioned              |                  |
```

## Appendix B — Full create request

```json
{
  "name": "bootstrap",
  "siteID": "00000000-0000-0000-0000-000000000000",
  "controlPlaneKubernetesVersion": "1.34.2",
  "cniPlugin": "calico",
  "etcdEncrypted": false,
  "controlPlaneVIP": "10.0.0.10",
  "capiInfraCluster": {
    "apiVersion": "infrastructure.cluster.x-k8s.io/v1alpha1",
    "kind": "PreprovisionedCluster",
    "metadata": { "name": "bootstrap" },
    "spec": {
      "controlPlaneEndpoint": { "host": "10.0.0.10", "port": 6443 },
      "hosts": [
        { "name": "host-01", "role": "control-plane",
          "addresses": [{ "type": "InternalIP", "address": "10.0.0.11" }] },
        { "name": "host-02", "role": "control-plane",
          "addresses": [{ "type": "InternalIP", "address": "10.0.0.12" }] },
        { "name": "host-03", "role": "control-plane",
          "addresses": [{ "type": "InternalIP", "address": "10.0.0.13" }] },
        { "name": "host-04", "role": "worker",
          "addresses": [{ "type": "InternalIP", "address": "10.0.0.21" }] }
      ]
    }
  },
  "nodePools": [
    {
      "name": "control-plane",
      "nodeNamePrefix": "cp",
      "desiredNodeCount": 3,
      "controlPlane": true,
      "systemReservedMemory": "1Gi",
      "capiSpireAttestationMethod": "joinToken",
      "capiInfraMachineTemplate": {
        "apiVersion": "infrastructure.cluster.x-k8s.io/v1alpha1",
        "kind": "PreprovisionedMachineTemplate",
        "spec": { "template": { "spec": {} } }
      }
    },
    {
      "name": "worker",
      "nodeNamePrefix": "w",
      "desiredNodeCount": 1,
      "controlPlane": false,
      "systemReservedMemory": "1Gi",
      "capiSpireAttestationMethod": "joinToken",
      "capiInfraMachineTemplate": {
        "apiVersion": "infrastructure.cluster.x-k8s.io/v1alpha1",
        "kind": "PreprovisionedMachineTemplate",
        "spec": { "template": { "spec": {} } }
      }
    }
  ]
}
```

Resulting objects in namespace `cluster-bootstrap`:

```
Cluster/bootstrap
PreprovisionedCluster/bootstrap
Machine/bootstrap-cp-a1b2               PreprovisionedMachine/bootstrap-cp-a1b2
  spec.infrastructureRef ---------------> spec.providerID:
                                            preprovisioned://bootstrap/host-01
                                          status.addresses: 10.0.0.11
Secret/bootstrap-cp-a1b2-bootstrap-data
```

## Appendix C — Generic-CAPI mode in CWE today

Entry condition, from `validateCreateClusterRequestFields` in
[`controller/internal/server/clusters.go`](../internal/server/clusters.go):

```
isCAPICluster := req.SiteID == uuid.Nil
```

| Field | CAPI cluster | Forge cluster |
|---|---|---|
| `capiInfraCluster` | Required | Rejected |
| `orgID`, `vpcID` | Rejected | Required |
| `capiInfraMachineTemplate` | Required per node pool | Rejected |
| `capiSpireAttestationMethod` | Required per node pool | Rejected |
| `instanceTypeID`, `subnetIDs`, `vpcPrefixIDs`, `osProfile`, `sshKeyGroupIDs`, `diskSelectionStrategy` | Rejected | Used |

Contract fields that Cluster API reads, from
`vendor/sigs.k8s.io/cluster-api/internal/contract/`:

| Object | Path | Use |
|---|---|---|
| Infrastructure cluster | `status.initialization.provisioned` | Gates every machine in the cluster |
| Infrastructure cluster | `spec.controlPlaneEndpoint` | Optional. Copied to the `Cluster` |
| Infrastructure machine | `status.initialization.provisioned` | Gates the machine |
| Infrastructure machine | `status.addresses` | Copied to `Machine.status.addresses` |
| Infrastructure machine | `spec.providerID` | Copied to `Machine.spec.providerID`. Links the node |

Fields that CWE itself reads from the infrastructure machine, in
[`controller/internal/temporal/activities/capi_node_resources.go`](../internal/temporal/activities/capi_node_resources.go):

| Path | Use |
|---|---|
| `status.addresses` | Fallback when the `Machine` has no address yet |
| `spec.instanceTypeID` | Optional. Absent gives no error |

## Appendix D — Comparison with the Nico provider

| | NicoCluster / NicoMachine | PreprovisionedCluster / PreprovisionedMachine |
|---|---|---|
| Creates infrastructure | Yes, through the Forge API | No |
| Cluster spec | `siteID`, `identityRef` | `hosts[]`, `controlPlaneEndpoint` |
| Machine spec | `vpcID`, `instanceTypeID`, `interfaces`, `ipxeScript`, `labels` | `providerID`, `hostSelector` |
| Source of the address | Reported by Forge after creation | Read from the inventory |
| Provider ID | `nico://<instance-id>` | `preprovisioned://<cluster>/<host>` |
| External dependency | Forge, Carbide | None |
| Who builds the machine spec | CWE, field by field, in `buildNicoMachine` | Cluster API, from the template |

The last row is the reason this design needs no CWE change. The Nico path is
template-less: CWE builds each `NicoMachine` from database fields. The generic
path renders the machine from the template that the caller supplied, so a new
kind needs no new CWE code.
