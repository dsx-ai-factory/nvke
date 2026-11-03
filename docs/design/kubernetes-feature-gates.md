# Kubernetes feature gates for workload clusters: design

> **Status:** proposal. The decision this design rests on is recorded in [controller/docs/adrs/20261102-workload-cluster-feature-gates.md](../../controller/docs/adrs/20261102-workload-cluster-feature-gates.md), also `proposed`.

## Summary

Operators can set Kubernetes feature gates when they create a workload cluster or a node pool: one map for the control-plane components, and one map for the kubelet on each set of nodes. The NVKE controller accepts only beta and GA gates that are valid for the Kubernetes version they will run on. It checks them in the API request, using a catalog generated from Kubernetes' own gate data, and rejects anything invalid with a 400 before it creates anything. Upgrades recheck the stored gates against the target version: a gate that no longer matters is dropped, and a gate whose loss would change behavior blocks the upgrade until the operator acknowledges it.

## Background

### Feature gates

A feature gate is a named switch, such as `SELinuxMount`, that turns a Kubernetes feature on or off. Components take them as `--feature-gates=Name=true,Other=false`. The kubelet also accepts them as `featureGates` in its configuration file. For every Kubernetes minor version, each gate has:

- **A stage:** alpha, beta, GA or deprecated. Alpha gates are off by default and can change or disappear in any release. Most beta gates are on by default, but not all: 11 of 93 on 1.36 are off. GA gates are on.
- **A default:** the value the component uses when the gate is not set.
- **A lock:** a gate locked to its default (`lockToDefault`) can't be set to the other value. Most GA gates are locked: 70 of 74 on 1.36.
- **Dependencies:** some gates need others on. On 1.36, `DRADeviceTaintRules` depends on `DRADeviceTaints`.

Gates are removed a few releases after they reach GA or are deprecated.

A component refuses to start when it is given a gate it doesn't know, a locked gate set to the other value, or an enabled gate whose dependencies are off. kube-apiserver, kube-controller-manager, kube-scheduler and the kubelet all register the same list of gates, so each accepts any gate on that list and acts only on the ones it uses.

### What NVKE does today

NVKE runs every component with the upstream defaults, and neither the REST API nor the management engine has a field to change a gate ([supported platforms](../overview/supported-platforms.md#kubernetes-feature-gates)). The only gate NVKE sets is `ImageVolume`, which `controller/internal/kubeadmconfig/render.go` turns on, on kube-apiserver and the kubelet, for versions before 1.36. That code already guards against passing a gate the version doesn't have, because a kube-apiserver that does not start leaves a cluster half built.

## Problem

An operator who needs a gate that is off by default has no supported way to turn it on. One who needs to turn off an on-by-default beta gate can't do that either. On 1.36, NVKE's supported version, these beta gates are off by default:

| Gate | 1.36 | 1.37 |
|---|---|---|
| `MaxUnavailableStatefulSet` | Beta, off | Beta, on |
| `SELinuxMount` | Beta, off | GA, on |
| `SchedulerAsyncAPICalls` | Beta, off | Beta, off |
| `DRADeviceTaintRules` | Beta, off | GA, on |

Three of the four change on the next minor version. Gates move every release, so any gate NVKE stores has to be rechecked whenever the cluster's version changes.

## Goals

- Operators set beta and GA gates at creation on a cluster's control-plane components, on its control-plane kubelets, and on each node pool's kubelets.
- Gates carry over through node replacement, scale-out and upgrades.
- A gate that would stop a component from starting is rejected in the request that introduced it, with a 400 that names the gate and the reason.
- An upgrade never changes the effective value of a gate the operator set unless the operator agreed to it in the upgrade request. Gates the operator didn't set follow the target version's upstream defaults, as they do on any Kubernetes upgrade; pinning one means setting it.

## Non-goals

- **Alpha gates.** They are rejected. Allowing them is a separate decision.
- **kube-proxy and etcd gates.** NVKE doesn't pass gates to either. A kube-proxy gate set in one of the maps is accepted and ignored; see [Validation](#validation).
- **Vetting what a gate does.** NVKE checks that a gate is valid for the version, not that the feature works with NVKE's nodes, drivers or images.
- **Changing gates on a running cluster or node pool.** See [Later work](#later-work).
- **kube-apiserver `--runtime-config`.** See [Later work](#later-work).

## API

### Cluster creation

`CreateClusterControlPlaneConfig` gets two optional maps from gate name to `true` or `false`:

```yaml
controlPlaneConfig:
  desiredCount: 3
  featureGates:            # kube-apiserver, kube-controller-manager and kube-scheduler
    MaxUnavailableStatefulSet: true
    SchedulerAsyncAPICalls: true
  kubeletFeatureGates:     # the kubelet on control-plane nodes
    SELinuxMount: true
```

All three control-plane components get the same `featureGates` map. Every component accepts every gate (see [Feature gates](#feature-gates)), so one map works. It also means operators don't need to know which component reads which gate, and a gate read by two components can't end up on in one and off in the other.

Control-plane nodes run a kubelet too, and NVKE configures it the same way as a node pool's kubelet. `kubeletFeatureGates` covers it.

### Node pool creation

`NodePoolCreateConfig` gets `kubeletFeatureGates`, with the same shape:

```yaml
kubeletFeatureGates:
  SELinuxMount: true
```

Each pool has its own map, so two pools in a cluster can set different gates.

### Reading them back

`Cluster.controlPlaneConfig` and `NodePool` return the stored maps. An empty or absent map means upstream defaults.

### Upgrade acknowledgement

`UpdateClusterRequest` and `UpdateNodePoolRequest` get `acknowledgedFeatureGates`, a list of gate names. It is accepted only in a request that changes the Kubernetes version. See [Acknowledging a gate](#acknowledging-a-gate).

## Validation

### Rules

The controller applies the operator's map on top of the version's defaults, then accepts it when, in the Kubernetes version it will run on:

1. every gate in the map exists and names a single feature,
2. every gate in the map is beta or GA,
3. no gate in the map is locked to the other value, and
4. every gate that ends up on has all its dependencies on.

The fourth rule looks at the result, not only at the map. It rejects turning on a gate without its dependencies, and it also rejects turning off a gate that an on-by-default gate depends on. That is the same check the components run at startup.

The first rule excludes `AllAlpha` and `AllBeta`. Every gate registry includes them, and `AllBeta` is itself registered as beta, but each one switches on a whole stage: `AllBeta=true` turns on every beta gate the operator didn't set, including beta gates a later version adds. The stored map would then no longer describe what the cluster runs, and an upgrade could turn on a new gate that never appears in the map or in an acknowledgement. The catalog leaves both out, so they fail as unknown gates.

On 1.36:

| Map | Result | Why |
|---|---|---|
| `SELinuxMount: true` | Accepted | Beta, off by default |
| `DRADeviceTaints: false` | Accepted | Beta and on by default, but not locked |
| `CPUManagerPolicyAlphaOptions: true` | Rejected | Alpha |
| `AllBeta: true` | Rejected | Switches on every beta gate, not a single feature |
| `AnyVolumeDataSource: false` | Rejected | GA and locked to `true` |
| `DRADeviceTaintRules: true`, `DRADeviceTaints: false` | Rejected | `DRADeviceTaintRules` depends on `DRADeviceTaints` |

An unlocked GA gate may be set to `false`. That is valid until the gate is locked, and [the upgrade that locks it](#upgrades) asks the operator to acknowledge the change.

Validation checks that a gate is valid for the version, not that a component the map reaches reads it. Kubernetes doesn't record which component reads which gate: every component registers the whole list and ignores the gates it doesn't use. So a gate only kube-proxy reads, such as `WinDSR`, is accepted in either map and has no effect, and so is a kubelet-only gate in `featureGates`. Filtering by component would mean classifying every gate by hand for every Kubernetes release, and an ignored gate can't stop a component from starting. The operator page lists this case.

A gate NVKE sets itself takes precedence. An operator value that contradicts it is rejected, and one that agrees with it is merged. On 1.36 and later NVKE sets no gates, so this applies only to `ImageVolume` below 1.36.

### When it runs

The check runs in the request handler, before the handler writes anything: no cluster or node pool record and no workflow outbox entry. An invalid gate leaves nothing to clean up.

| Request | Gates checked | Against |
|---|---|---|
| Create cluster | `featureGates`, `kubeletFeatureGates` | The cluster's Kubernetes version |
| Create node pool | The pool's `kubeletFeatureGates` | The pool's Kubernetes version |
| Upgrade the control plane | The stored control-plane maps | The target version |
| Upgrade a node pool | The pool's stored map | The target version |

Each pool is checked against its own version, because pools can run up to three minor versions behind the control plane.

A failure returns 400 with the existing `GenericError` body. The message lists every invalid gate and the reason, so the operator can fix them all in one go:

```json
{
  "message": "invalid feature gates for Kubernetes 1.36: controlPlaneConfig.featureGates.CPUManagerPolicyAlphaOptions: alpha gates are not supported; controlPlaneConfig.kubeletFeatureGates.AnyVolumeDataSource: locked to true"
}
```

The management engine creates clusters and node pools through this REST API (`management-engine/internal/nvkecontroller/client.go`), so the same checks cover it.

## The catalog

### What it holds

For every Kubernetes minor version NVKE can create a cluster on or upgrade one to, the catalog lists every gate with its stage, default, lock and dependencies. Today that is 1.36, and 1.37 for upgrades. Validation reads nothing else, and never calls a cluster.

Kubernetes rarely changes a gate in a patch release, so the catalog has one entry per minor version. Each entry is generated from the release tag of the patch version NVKE supports, for example `v1.36.3`.

### Where the data comes from

The generator loads Kubernetes' own gate registry, the one every component uses to accept or reject a gate. It builds `k8s.io/kubernetes/pkg/features` at that tag, in a Go module of its own so the controller takes no dependency on `k8s.io/kubernetes`, and reads two things from the registry:

- **`GetAllVersioned()`**: every gate's stage, default and lock, as a history. The registry includes the API server's gates.
- **`Dependencies()`**: which gates each gate needs, including gates of the API server.

It leaves out `AllAlpha` and `AllBeta`, which the registry reports alongside the real gates (see [Validation](#validation)).

Kubernetes writes the same history to `test/compatibility_lifecycle/reference/versioned_feature_list.yaml` in its repository, which is the easiest way to read it by hand.

The generator resolves each minor version the way a component on that version picks a spec. A component chooses using two settings: its emulation version and its minimum compatibility version. NVKE sets neither `--emulation-version` nor `--min-compatibility-version`, so every component runs with the defaults: the emulation version is the component's own minor version, and the minimum compatibility version is one minor below it. For 1.37 that is emulation 1.37 and minimum compatibility 1.36. The spec in effect is the last entry whose `version` is not newer than the emulation version and whose `minCompatibilityVersion`, if it has one, is not newer than the minimum compatibility version. Kubernetes uses `minCompatibilityVersion` to add a second spec at the same version that changes only the default, and that spec takes effect only once every component in the control plane is new enough. A 1.37 component with the default flags therefore uses the earlier spec for a 1.37 gate whose second spec requires minimum compatibility 1.37. A later design that lets operators set either flag would need a catalog entry per setting, not one per minor version.

For gates without `minCompatibilityVersion`, which is most of them, this is the last entry whose `version` is not newer than the minor version:

  ```yaml
  - name: SELinuxMount
    versionedSpecs:
    - default: false
      lockToDefault: false
      preRelease: Alpha
      version: "1.30"
    - default: false            # in effect for 1.33 through 1.36
      lockToDefault: false
      preRelease: Beta
      version: "1.33"
  ```

A gate missing from a version's registry has been removed in that version.

### How it's maintained

The generator lives under `controller/hack/` and writes one data file per minor version into a package under `controller/internal/`. The data files are committed. A `make` target regenerates them, and a check target fails when the committed files differ from a fresh run, like `generate-check` for the OpenAPI code.

Adding a Kubernetes version that clusters can be created on or upgraded to includes regenerating the catalog and reviewing the diff. A version with no catalog entry has no valid gates, so requests with gates for it fail instead of passing unchecked.

## Rendering

### Control plane

At `kubeadm init`, the renderer adds `feature-gates` to the `extraArgs` of kube-apiserver, kube-controller-manager and kube-scheduler. The value is the operator's map merged with any gate NVKE sets, as one comma-separated list sorted by name so the output is stable for golden-file tests. `extraArgs` is already built as a map and converted to the list form v1beta4 needs, so no conversion code changes.

kubeadm stores this configuration in the cluster's `kube-system/kubeadm-config` ConfigMap. Control-plane nodes that join later, for scale-out, replacement or an upgrade, take their configuration from that ConfigMap, not from a fresh render. So the gates follow every control-plane node without extra work. The OIDC and etcd encryption rollouts write their settings into the same ConfigMap for the same reason.

### Kubelet

Every node gets a kubelet patch at `/etc/kubernetes/patches/kubeletconfiguration0+merge.yaml`, rendered when the node joins. It gets a `featureGates` map: from the pool's `kubeletFeatureGates`, or the control plane's for a control-plane node. Because the patch is rendered from the database on every join, replacement, scale-out and upgraded nodes all get the current gates.

## Upgrades

### What gets rechecked

A control-plane upgrade rechecks the control plane's `featureGates` and `kubeletFeatureGates` against the target version. A node pool upgrade rechecks that pool's `kubeletFeatureGates`. The check runs in the update handler, next to the existing version-skew checks, before the handler writes anything.

### What happens to each gate

The rule is: keep a gate while the target version still accepts it, drop it when dropping it changes nothing, and ask the operator when dropping it would change behavior. For each stored gate, the controller compares the operator's value with the value the target version would use without it:

| The stored gate in the target version | Behavior changes? | Result |
|---|---|---|
| Still valid, including beta that became GA | No | Kept |
| Now deprecated, and still settable | No, it still works | Kept. The upgrade that removes or locks it checks it again |
| Removed or locked, and the target version uses the stored value anyway | No | Dropped |
| Removed or locked, and the target version uses the other value | Yes | 400 until acknowledged |
| Fails the dependency rule | Yes | 400 until acknowledged |

For a removed gate, "the value the target version uses" is its default in the cluster's current version, the last version that had it.

A deprecated gate can't be set in a new request, but one already stored is kept while it still works. Dropping it early would change the cluster's behavior for no reason.

A dropped gate disappears from the stored maps, so the `200` response to the upgrade shows the result.

Upgrading from 1.36 to 1.37:

| Stored gate | On 1.37 | Result |
|---|---|---|
| `SELinuxMount: true` | GA, on, not locked | Kept |
| `SchedulerAsyncAPICalls: true` | Beta, off | Kept |
| `DRAExtendedResource: true` | GA, locked to `true` | Dropped: 1.37 is always on, as the operator asked |
| `DRAExtendedResource: false` | GA, locked to `true` | 400: the upgrade would turn the feature on |

### Acknowledging a gate

Acknowledgement needs no state between requests. The operator sends the upgrade, reads the 400, and sends the same upgrade again with the gates named:

```http
PATCH /api/clusters/{clusterID}
{"controlPlaneKubernetesVersion": "1.37.2"}
```

```http
400 Bad Request
{"message": "upgrading to Kubernetes 1.37 changes feature gates: controlPlaneConfig.featureGates.DRAExtendedResource=false is locked to true in 1.37, so the feature will be on. Resend with acknowledgedFeatureGates to accept this."}
```

```http
PATCH /api/clusters/{clusterID}
{"controlPlaneKubernetesVersion": "1.37.2", "acknowledgedFeatureGates": ["DRAExtendedResource"]}
```

The second request returns `200`. The upgrade starts, and the returned cluster no longer has `DRAExtendedResource` in its maps. A node pool upgrade works the same way, through `PATCH` on the node pool with its `kubernetesVersion`.

- A name applies to every map in the request's scope that holds the gate: on a control-plane upgrade, `featureGates` and `kubeletFeatureGates`. The gate means the same thing in both, and the target version treats it the same way.
- Acknowledging a gate always drops it. It doesn't keep the old value; the target version can't run with it.
- A name that needs no acknowledgement is ignored, so a client can resend the same list safely. A misspelled name doesn't acknowledge anything, so the request still gets the 400.
- `acknowledgedFeatureGates` is rejected in a request that doesn't change the Kubernetes version.

In this phase, acknowledging is the only way to change a cluster's gates after creation.

### Applying the result

**Control plane.** The upgrade reconciler (`controller/internal/controllers/controlplane/k8s_upgrade.go`) already writes the target version into `kubeadm-config` before it starts replacing control-plane nodes. It does that through the `UpdateKubeadmConfigMap` workflow, which calls `EnsureKubernetesVersionInClusterConfiguration` in `controller/internal/kubeadmconfig/reconcile.go`. In the same step, it recomputes `feature-gates` for the target version from the stored map and the gates NVKE sets on that version, and replaces the old value rather than adding to it, so a pre-1.36 cluster's `ImageVolume=true` is dropped. It uses the same `reconcileClusterConfiguration` helper, which already handles both the v1beta3 and v1beta4 shapes. The rest of the upgrade is unchanged: new-version nodes replace old ones, then `kubeadm upgrade apply` upgrades the add-ons.

**Node pool.** The stored map is updated in the same transaction as the pool's version. Replacement nodes render their kubelet patch from it.

## Security considerations

Setting gates needs the same permission as creating the cluster or node pool, and a gate affects only that cluster. Alpha gates, which can change or disappear in any release, are rejected.

## Testing

- **Catalog:** table-driven unit tests for each rule against a small fixture catalog, including the dependency cases. Another test loads every committed version.
- **Generator:** the check target fails CI when the committed data drifts from what the generator produces.
- **Rendering:** golden-file tests for the init configuration and the kubelet patch, for the v1beta3 and v1beta4 shapes, with and without NVKE's own gates.
- **Handlers:** unit tests that an invalid gate returns 400 and writes nothing, and that an upgrade keeps, drops or rejects each gate as [the table](#what-happens-to-each-gate) says.
- **End to end:** on a local cluster through Tilt and CAPD, the flags appear on the control-plane static pods, and `featureGates` appears in each node's kubelet configuration.

## Delivery

Each step is its own pull request, in this order:

1. The catalog, its generator and the validation rules, with no API change.
2. The control-plane fields at cluster creation, with rendering.
3. The kubelet fields, on the control plane and on node pools.
4. Rechecking stored gates on upgrade, with `acknowledgedFeatureGates`.
5. The documentation and a demo script. See [Documentation](#documentation).

Steps 2 and 3 both change `openapi.yaml` and add a database migration, so they land one after the other.

## Later work

These are outside this design. Each needs its own design before work starts.

- **Changing gates on a running cluster or node pool.** The upgrade path already writes `kubeadm-config` and replaces nodes, so this would reuse it: validate against the current version, write the new gates, then replace nodes, control plane first.
- **kube-apiserver `--runtime-config`.** Some gates also need an API version that is off by default. On 1.36, a `DeviceTaintRule` needs `DRADeviceTaintRules` and `resource.k8s.io/v1beta2=true`. On 1.37 it needs neither. It would follow the same rules: beta API versions only, checked against the version.
- **Management engine fields.** Clusters and node pools the management engine manages get gates only once `NVKECluster` and `NodePool` pass them through to the REST API.
- **Alpha gates.**

## Documentation

- A new page, `docs/workload-clusters/feature-gates.md`, for operators. It covers setting gates on the control plane and node pools, which gates are allowed, what each validation error means, that a gate no configured component reads (such as a kube-proxy gate) is accepted but has no effect, and what happens on upgrade, including how to acknowledge a gate. Its example uses a beta gate that is off by default on 1.36.
- The [supported platforms](../overview/supported-platforms.md#kubernetes-feature-gates) page currently says NVKE sets no gates and has no field for them. It changes to link the new page. Its statement that NVKE enables no alpha gates stays true.
- A `demo.sh` that creates a cluster and a node pool with gates, shows them on the control plane and the kubelet, and shows an invalid gate and an alpha gate being rejected. Where it lives is open for review, because the repository has no convention for demo scripts.
