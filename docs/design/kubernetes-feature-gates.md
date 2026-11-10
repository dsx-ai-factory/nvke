# Kubernetes feature gates for workload clusters: design

> **Status:** proposal. The decision this design rests on is recorded in [controller/docs/adrs/20261102-workload-cluster-feature-gates.md](../../controller/docs/adrs/20261102-workload-cluster-feature-gates.md), also `proposed`.

## Summary

Operators can set Kubernetes feature gates when they create a workload cluster or a node pool: one map for the control-plane components, and one map for the kubelet in each node pool. The NVKE controller accepts only beta and GA gates that are valid for the Kubernetes version they will run on. It checks them in the API request, using a catalog generated from Kubernetes' own gate data, and rejects anything invalid with a 400 before it creates anything. Upgrades recheck the stored gates against the target version: a gate that no longer matters is dropped, and a gate whose loss would change behavior blocks the upgrade until the operator acknowledges it.

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

Control-plane nodes are bootstrapped with `kubeadm init` and `kubeadm join`. kubeadm keeps the control plane's configuration in the workload cluster, in the `kube-system/kubeadm-config` ConfigMap, and a control-plane node that joins later reads its component flags from there. Every node that joins, worker or control plane, also takes its kubelet configuration from the `kube-system/kubelet-config` ConfigMap written at `kubeadm init`, with the patch files NVKE writes merged over it. `kubeadm join` ignores the `KubeletConfiguration` document NVKE renders into the join file.

The controller keeps each control-plane configuration as an append-only snapshot in `control_plane_configurations`, which includes the Kubernetes version. Each control-plane node is pinned to the snapshot it was built from, and a version upgrade appends a new snapshot. A node pool works the same way through node sets: each node set copies the pool's Kubernetes version and other node settings, a change to any of them creates a new node set, and each worker belongs to the node set it was built from. A node set is deleted once its last node is gone.

A separate control-plane isolation design, still in draft, replaces this. NVKE renders a configuration file for each control-plane host, and the node agent runs individual `kubeadm init phase` commands from it. Control-plane hosts then run a standalone kubelet that registers no `Node` and runs only their static pods, and workers start their kubelet without `kubeadm join`. Nothing is read back from the workload cluster. This design works with both; see [Rendering](#rendering).

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

- Operators set beta and GA gates at creation on a cluster's control-plane components and on each node pool's kubelets.
- Gates carry over through node replacement, scale-out and upgrades.
- A gate that would stop a component from starting is rejected in the request that introduced it, with a 400 that names the gate and the reason.
- An upgrade never changes the effective value of a gate the operator set unless the operator agreed to it in the upgrade request. Gates the operator didn't set follow the target version's upstream defaults, as they do on any Kubernetes upgrade; pinning one means setting it.

## Non-goals

- **Alpha gates.** They are rejected. Allowing them is a separate decision.
- **kube-proxy and etcd gates.** NVKE doesn't pass gates to either. A kube-proxy gate set in one of the maps is accepted and ignored; see [Validation](#validation).
- **Kubelet gates on control-plane hosts.** See [Cluster creation](#cluster-creation).
- **Vetting what a gate does.** NVKE checks that a gate is valid for the version, not that the feature works with NVKE's nodes, drivers or images.
- **Changing gates on a running cluster or node pool.** See [Later work](#later-work).
- **kube-apiserver `--runtime-config`.** See [Later work](#later-work).

## API

### Cluster creation

`CreateClusterControlPlaneConfig` gets `featureGates`, an optional map from gate name to `true` or `false`:

```yaml
controlPlaneConfig:
  desiredCount: 3
  featureGates:            # kube-apiserver, kube-controller-manager and kube-scheduler
    MaxUnavailableStatefulSet: true
    SchedulerAsyncAPICalls: true
```

All three control-plane components get the same `featureGates` map. Every component accepts every gate (see [Feature gates](#feature-gates)), so one map works. It also means operators don't need to know which component reads which gate, and a gate read by two of them can't end up on in one and off in the other.

The control plane has no kubelet gate map. Under control-plane isolation, a control-plane host runs a standalone kubelet that registers no `Node` and runs only that host's static pods. Kubelet gates such as `SELinuxMount` or the DRA gates act on workload pods, so on that kubelet they would do nothing, and a bad one could only stop it. NVKE still sets its own kubelet gates there; see [Gates NVKE sets or requires](#gates-nvke-sets-or-requires). With one map, `controlPlaneConfig.featureGates` can't be mistaken for kubelet gates, so it needs no longer name.

The map is stored in the control-plane configuration snapshot, next to the Kubernetes version, so every snapshot records the version and the gates together.

### Node pool creation

`NodePoolCreateConfig` gets `kubeletFeatureGates`, with the same shape:

```yaml
kubeletFeatureGates:
  SELinuxMount: true
```

Each pool has its own map, so two pools in a cluster can set different gates. The map is stored on the pool and copied into each node set next to the Kubernetes version, the same way as the control-plane map and its snapshot.

### Reading them back

`Cluster.controlPlaneConfig.featureGates` and `NodePool.kubeletFeatureGates` return the stored maps. An empty or absent map means upstream defaults.

### Upgrade acknowledgement

`UpdateClusterRequest` and `UpdateNodePoolRequest` get `acknowledgedFeatureGates`, a list of gate names. It is accepted only in a request that changes the Kubernetes version. See [Acknowledging a gate](#acknowledging-a-gate).

## Validation

### Rules

The controller applies the operator's map on top of the version's defaults, then accepts it when, in the Kubernetes version it will run on:

1. every gate in the map exists and names a single feature,
2. every gate in the map is beta or GA,
3. no gate in the map is locked to the other value,
4. every gate that ends up on has all its dependencies on, and
5. no gate NVKE requires is set to `false`, and no gate NVKE sets is set to the other value.

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
| `ImageVolume: false` | Rejected | GA and locked to `true`. NVKE relies on it, but the lock already rules it out |

An unlocked GA gate may be set to `false`. That is valid until the gate is locked, and [the upgrade that locks it](#upgrades) asks the operator to acknowledge the change.

Validation checks that a gate is valid for the version, not that a component the map reaches reads it. Kubernetes doesn't record which component reads which gate: every component registers the whole list and ignores the gates it doesn't use. So a gate only kube-proxy reads, such as `WinDSR`, is accepted in either map and has no effect, and so is a kubelet-only gate in `featureGates`. Filtering by component would mean classifying every gate by hand for every Kubernetes release, and an ignored gate can't stop a component from starting. Nothing is removed from the map either: all three control-plane components get every gate in `featureGates`, and each acts on the ones it reads. The operator page lists the commonly used gates for each map; see [Documentation](#documentation).

A feature that needs both the control plane and the kubelet, which includes most DRA features, has to be set in `featureGates` and in the `kubeletFeatureGates` of every node pool that uses it. Validation checks each map on its own, and for the same reason it can't tell which features span both. A pool's map is checked against the pool's version, which can be older than the control plane's.

### Gates NVKE sets or requires

**Gates NVKE sets.** NVKE sets some gates itself, on the control-plane components and on every kubelet, including the standalone kubelet on control-plane hosts. Today that is only `ImageVolume`, below 1.36. NVKE's value takes precedence. An operator value that contradicts it is rejected, and one that agrees with it is merged.

**Gates NVKE requires.** NVKE also relies on gates it leaves at their upstream default. A locked gate needs no extra handling, because rule 3 already rejects changing it: on 1.36, `ImageVolume` is GA and locked to `true`. An unlocked default-on gate could be turned off, though. On 1.36 that includes DRA betas such as `DRAPartitionableDevices` and `DRAConsumableCapacity`. So NVKE keeps a short list of gates it requires, per minor version, next to the catalog, and rule 5 rejects setting one to `false` with the same 400. The list is maintained by hand. It stays short because it holds only unlocked gates NVKE's own components or GPU stack depend on, and the operator page publishes it. Which gates belong on it is an [open question](#open-questions).

### When it runs

The check runs in the request handler, before the handler writes anything: no cluster or node pool record and no workflow outbox entry. An invalid gate leaves nothing to clean up.

| Request | Gates checked | Against |
|---|---|---|
| Create cluster | `featureGates` | The cluster's Kubernetes version |
| Create node pool | The pool's `kubeletFeatureGates` | The pool's Kubernetes version |
| Upgrade the control plane | The stored `featureGates` | The target version |
| Upgrade a node pool | The pool's stored map | The target version |

Each pool is checked against its own version, because pools can run up to three minor versions behind the control plane.

A failure returns 400 with the existing `GenericError` body. The message lists every invalid gate and the reason, so the operator can fix them all in one go:

```json
{
  "message": "invalid feature gates for Kubernetes 1.36: controlPlaneConfig.featureGates.CPUManagerPolicyAlphaOptions: alpha gates are not supported; controlPlaneConfig.featureGates.AnyVolumeDataSource: locked to true"
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

The stored maps are the only source of gates. Each host gets its gates rendered from them for the Kubernetes version that host runs, merged with the gates NVKE sets for that version. NVKE's value takes precedence, as in [Validation](#validation). The rendered value is one list sorted by gate name, so golden-file output is stable.

Today's bootstrap and control-plane isolation both meet this. They differ only in where the rendered gates go.

### Control plane

The gates go into the `extraArgs` of kube-apiserver, kube-controller-manager and kube-scheduler in the `ClusterConfiguration`, as one `feature-gates` entry. `extraArgs` is already built as a map and converted to the list form v1beta4 needs, so no conversion code changes. They don't go in `ClusterConfiguration.featureGates`, which holds kubeadm's own gates.

Each control-plane host renders from the snapshot it is pinned to, which holds that host's version and gates.

**Today.** The renderer adds the entry at `kubeadm init`, and kubeadm stores the configuration in `kube-system/kubeadm-config`. Control-plane nodes that join later, for scale-out, replacement or an upgrade, take their flags from that ConfigMap, not from a fresh render. The OIDC and etcd encryption rollouts write their settings into the same ConfigMap for the same reason. The upgrade rewrites the entry there; see [Applying the result](#applying-the-result).

**Control-plane isolation.** Each control-plane host gets its own configuration file, rendered by NVKE and written by the node agent. `kubeadm init phase control-plane all` writes the static pod manifests from it. Every host's file is rendered from its pinned snapshot, so scale-out, replacement and upgraded hosts get the current gates with no ConfigMap step, and a host still on the old version keeps its own.

Either way, the standalone or registered kubelet on a control-plane host gets only the kubelet gates NVKE sets for its version (today `imageVolumeKubeletFeatureGates` in `render.go`, which returns `ImageVolume` below 1.36). Today they reach it the same way as a worker's; see below.

### Node pool kubelets

A node's kubelet `featureGates` is the `kubeletFeatureGates` of its node set, merged with the kubelet gates NVKE sets for the node set's version. It is rendered from the database every time a node joins, so replacement, scale-out and upgraded nodes all get their node set's gates. The version is the node set's, not the cluster's: the join activity (`controller/internal/temporal/activities/kubeadm_join.go`) passes `cluster.KubernetesVersion` today, which is the control plane's version and can be up to three minors ahead of the pool.

**Today.** `kubeadm join` builds the kubelet configuration from `kubelet-config` and the patch files, not from the join file, so the gates go into a patch. A merge patch would only add keys: a gate already in `kubelet-config` would stay, whether a customer with cluster-admin put it there or `kubeadm init` did, like the `ImageVolume=true` of a cluster created before 1.36. So the gates go into a separate JSON patch, `/etc/kubernetes/patches/kubeletconfiguration1+json.yaml`, with one `add` operation on `/featureGates`. kubeadm applies it after `kubeletconfiguration0+merge.yaml`, and it replaces the whole map. NVKE writes it on every node, at `kubeadm init` and every `kubeadm join`, even when the map is empty, so an empty map clears what `kubelet-config` holds. NVKE's own kubelet gates move into this patch, and the copies in the init and join documents are removed so there is one source. `kubeadm upgrade apply` already passes `--patches` with the same directory, so it keeps the gates when it rewrites the kubelet configuration on a control-plane host.

**Control-plane isolation.** Workers start their kubelet without `kubeadm join`, from a `KubeletConfiguration` NVKE renders in full, and the gates go into it directly. This assumes that configuration is rendered from the worker's node set in the database, as the patch is today; see [Open questions](#open-questions).

## Upgrades

### What gets rechecked

A control-plane upgrade rechecks the control plane's `featureGates` against the target version. A node pool upgrade rechecks that pool's `kubeletFeatureGates`. The check runs in the update handler, next to the existing version-skew checks, before the handler writes anything.

### What happens to each gate

The rule is: keep a gate while the target version still accepts it, drop it when dropping it changes nothing, and ask the operator when dropping it would change behavior. For each stored gate, the controller compares the operator's value with the value the target version would use without it:

| The stored gate in the target version | Behavior changes? | Result |
|---|---|---|
| Still valid, including beta that became GA | No | Kept |
| Now deprecated, and still settable | No, it still works | Kept. The upgrade that removes or locks it checks it again |
| Removed or locked, and the target version uses the stored value anyway | No | Dropped |
| Removed or locked, and the target version uses the other value | Yes | 400 until acknowledged |
| Fails the dependency rule | Yes | 400 until acknowledged |
| Stored as `false` and NVKE requires it on the target version, or NVKE sets the other value there | Yes | 400 until acknowledged |

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

### When the dependency rule fails

A dependency failure can involve more than one stored gate, so the 400 names all of them. Take a stored `A: true`, where the target version makes `A` depend on `B`:

- If `B` isn't stored, the 400 names `A`. `B` is off by default on the target, and dropping `A` fixes it.
- If the operator also stored `B: false`, the 400 names `A` and `B`. Dropping `A` fixes it if `A` is off by default on the target. Dropping `B` fixes it if `B` is on by default. Only the operator knows which they want.

The operator acknowledges whichever gates they choose. The controller drops them, then runs every [validation rule](#rules) on the result. If it still fails, the upgrade gets another 400 naming what is still wrong, and nothing is written. Dropping every gate named always resolves the failure, because each Kubernetes version's defaults satisfy its own dependencies, but the operator doesn't have to drop them all.

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

- A name applies to the map the request upgrades: `featureGates` on a control-plane upgrade, or the pool's `kubeletFeatureGates` on a node pool upgrade.
- Acknowledging a gate always drops it. It doesn't keep the old value; the target version can't run with it.
- A name that needs no acknowledgement is ignored, so a client can resend the same list safely. A misspelled name doesn't acknowledge anything, so the request still gets the 400.
- `acknowledgedFeatureGates` is rejected in a request that doesn't change the Kubernetes version.

In this phase, acknowledging is the only way to change a cluster's gates after creation.

### Applying the result

The stored map is updated in the same transaction as the target version. For the control plane, the update handler already appends a snapshot with the target version, and the rechecked map goes into that snapshot. For a node pool, the map is updated on the pool, together with the [rollback record](#rollback), and the node pool controller copies it into the new node set it creates for the target version. Dropped and acknowledged gates leave the map before any node is replaced.

**Control plane, today.** The upgrade reconciler (`controller/internal/controllers/controlplane/k8s_upgrade.go`) already writes the target version into `kubeadm-config` before it starts replacing control-plane nodes. It does that through the `UpdateKubeadmConfigMap` workflow, which calls `EnsureKubernetesVersionInClusterConfiguration` in `controller/internal/kubeadmconfig/reconcile.go`. In the same step, it recomputes `feature-gates` for the target version from the stored map and the gates NVKE sets on that version, and replaces the old value rather than adding to it, so a pre-1.36 cluster's `ImageVolume=true` is dropped. It uses the same `reconcileClusterConfiguration` helper, which already handles both the v1beta3 and v1beta4 shapes. The rest of the upgrade is unchanged: new-version nodes replace old ones, then `kubeadm upgrade apply` upgrades the add-ons.

**Control plane, under isolation.** There is no ConfigMap to update. Each new-version host renders its configuration file from the new snapshot. A host still on the old version stays pinned to the old snapshot until it is replaced. If its file is rewritten before then, for example to update etcd membership, the rewrite renders from that snapshot too. Otherwise a phase re-run there would start old-version components with the target version's gates.

**Node pool.** Replacement nodes render their kubelet configuration from the new node set, under either bootstrap. Nodes still in the old node set keep its version and gates until they are replaced.

### Rollback

NVKE doesn't support rolling back a Kubernetes version today, but rollback is a planned requirement. When it arrives, it has to restore the gates along with the version. A gate dropped on upgrade because the target version used the same value may have had a different default on the older version. An acknowledged gate was valid there.

So the map the upgrade replaced has to be kept with the version it upgraded from. Rolling back restores both together, and the restored map is valid on that version because it was valid there before the upgrade.

- **Control plane:** the snapshot the nodes were pinned to before the upgrade already holds both, so nothing new is recorded.
- **Node pool:** the old node set holds both only until its last node is replaced, then it is deleted, so the upgrade records the replaced map and version in the same transaction that writes the target version. Where the record lives, and how many upgrades it goes back, is for the rollback design. This design only requires that the upgrade writes it.

## Security considerations

Setting gates needs the same permission as creating the cluster or node pool, and a gate affects only that cluster. Alpha gates, which can change or disappear in any release, are rejected.

With today's bootstrap, the control-plane gates live in `kube-system/kubeadm-config` inside the workload cluster, where a customer with cluster-admin can edit them. A control-plane node that joins later would start with whatever is there, including gates NVKE never validated. Control-plane isolation closes this: every host renders from NVKE's database, and nothing is read back from the cluster. The same customer can edit `kubelet-config` today, but the JSON patch replaces its whole `featureGates` map, so kubelet gates come only from the database under both. The rest of `kubelet-config` stays editable until isolation.

## Testing

- **Catalog:** table-driven unit tests for each rule against a small fixture catalog, including the dependency cases and the required list. Another test loads every committed version.
- **Generator:** the check target fails CI when the committed data drifts from what the generator produces.
- **Rendering:** golden-file tests for the init configuration and the kubelet JSON patch, for the v1beta3 and v1beta4 shapes, with and without NVKE's own gates. A test applies the patch over a kubelet configuration that holds a stale gate and checks the gate is gone. A control-plane host pinned to an older snapshot renders that snapshot's version and gates, and a worker renders its node set's version, not the cluster's. Control-plane isolation adds golden files for its per-host configuration file and worker kubelet configuration, which fail if its renderer leaves the gates out.
- **Handlers:** unit tests that an invalid gate returns 400 and writes nothing, and that an upgrade keeps, drops or rejects each gate as [the table](#what-happens-to-each-gate) says. They cover a dependency failure with two stored gates, acknowledging either one, and acknowledging one that doesn't fix it. They check that a control-plane upgrade puts the rechecked map in the new snapshot, and that a node pool upgrade writes the rollback record.
- **End to end:** on a local cluster through Tilt and CAPD, the flags appear on the control-plane static pods, and `featureGates` appears in each worker's kubelet configuration.

## Delivery

Each step is its own pull request, in this order:

1. The catalog, its generator, the required list and the validation rules, with no API change.
2. The control-plane `featureGates` at cluster creation, with rendering for today's bootstrap.
3. The node pool `kubeletFeatureGates`, with rendering.
4. Rechecking stored gates on upgrade, with `acknowledgedFeatureGates` and the rollback record.
5. The documentation and a demo script. See [Documentation](#documentation).

Steps 2 and 3 both change `openapi.yaml`, so they land one after the other. Step 3 also adds the map to node pools and node sets in a database migration. Until step 4 lands, the update handlers reject a Kubernetes version change on a cluster or node pool with stored gates, with a 400 saying upgrades with feature gates aren't supported yet. Without that, an upgrade would carry the gates to a version nobody checked them against. Step 4 removes the guard.

Control-plane isolation is delivered separately. Whichever lands second carries the gates into the isolation renderer, and its golden files show they are there.

## Later work

These are outside this design. Each needs its own design before work starts.

- **Changing gates on a running cluster or node pool.** This gives existing clusters the same gates as new ones. The storage in this design already supports it, and it applies new gates the way an upgrade does. A `PATCH` validates the new map against the current version. For the control plane, it appends a snapshot, and `featureGates` becomes one of the fields that requires a rollout (`ComputeControlPlaneRolloutChanges` in `controller/internal/db/control_plane_configurations.go`), so pinned nodes are replaced. For a node pool, the gates join the fields that create a new node set (`NodePoolNeedsNewNodeSet` in `controller/internal/db/node_sets.go`). The control plane goes first, so a feature that spans both maps isn't on in the kubelets before the API server. Until then, an existing cluster can get kubelet gates only through a new node pool, and has no way to get control-plane gates.
- **kube-apiserver `--runtime-config`.** Some gates also need an API version that is off by default. On 1.36, a `DeviceTaintRule` needs `DRADeviceTaintRules` and `resource.k8s.io/v1beta2=true`. On 1.37 it needs neither. It would follow the same rules: beta API versions only, checked against the version.
- **Management engine fields.** Clusters and node pools the management engine manages get gates only once `NVKECluster` and `NodePool` pass them through to the REST API.
- **Alpha gates.**

## Open questions

- **Which gates NVKE requires.** The mechanism is above. The list needs the owners of NVKE's GPU stack to say which unlocked default-on gates it depends on, starting with the DRA betas on 1.36.
- **Control-plane isolation.** That design is still in draft, and this one assumes three things of it:
  - every render of a control-plane host's configuration file, including a rewrite after bootstrap, reads the snapshot that host is pinned to;
  - a worker's `KubeletConfiguration` is rendered by NVKE from the database for its node set on every join, not taken from a control-plane host's file;
  - an upgrade regenerates a host's static pods only from that host's file. Today `kubeadm upgrade apply` regenerates them from `kubeadm-config`, which isolation removes.

  Because the snapshots and node sets are the source of gates under both bootstraps, moving an existing cluster to isolation needs no extra step for its gates.

## Documentation

- A new page, `docs/workload-clusters/feature-gates.md`, for operators. It covers:
  - setting gates on the control plane and node pools, and which gates are allowed;
  - a short table of commonly used gates and the map each one goes in, without trying to list every gate;
  - the gates NVKE requires, which can't be set to `false`;
  - setting a feature that needs both the control plane and the kubelet in both maps;
  - that a gate no configured component reads, such as a kube-proxy gate, is accepted but has no effect;
  - what each validation error means;
  - what happens on upgrade, including how to acknowledge a gate.

  Its example uses a beta gate that is off by default on 1.36.
- The [supported platforms](../overview/supported-platforms.md#kubernetes-feature-gates) page currently says NVKE sets no gates and has no field for them. It changes to link the new page. Its statement that NVKE enables no alpha gates stays true.
- A `demo.sh` that creates a cluster and a node pool with gates, shows them on the control-plane components and a worker's kubelet, and shows an invalid gate and an alpha gate being rejected. Where it lives is open for review, because the repository has no convention for demo scripts.
