# Kubernetes feature gates for workload clusters: design

> **Status:** proposal. The decision this design rests on is recorded in [controller/docs/adrs/20261102-workload-cluster-feature-gates.md](../../controller/docs/adrs/20261102-workload-cluster-feature-gates.md), also `proposed`.

## Summary

Platform operators set Kubernetes feature gates on a `ControlPlaneClass`, one map per control-plane component, and every cluster that selects the class gets them. The management engine passes the maps to the NVKE controller unchanged, and the controller renders each one onto its component, as given. Whether a gate is valid for the Kubernetes version, and whether it works with the rest of the system, is the caller's responsibility. The controller rejects only a gate that contradicts one NVKE sets itself. Gates are stored with the Kubernetes version in each control-plane snapshot, so they carry through node replacement, scale-out and upgrades, and a change to them rolls the control plane like any other change to a snapshot.

Node pool kubelet gates use the same model and are still under review; see [Node pools](#node-pools).

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

The management engine creates and updates clusters through the controller's REST API (`management-engine/internal/nvkecontroller/client.go`). An `NVKECluster` selects a `ControlPlaneClass`, which holds the platform operator's control-plane settings, and a release channel, which decides the Kubernetes version. On every reconcile, the engine builds the control-plane request from the class and sends the controller an update for any field that differs from what the controller reports (`ensureCluster` in `management-engine/controllers/nvkecluster_controller.go`).

Control-plane nodes are bootstrapped with `kubeadm init` and `kubeadm join`. kubeadm keeps the control plane's configuration in the workload cluster, in the `kube-system/kubeadm-config` ConfigMap, and a control-plane node that joins later reads its component flags from there. Every node that joins, worker or control plane, also takes its kubelet configuration from the `kube-system/kubelet-config` ConfigMap written at `kubeadm init`, with the patch files NVKE writes merged over it. `kubeadm join` ignores the `KubeletConfiguration` document NVKE renders into the join file.

The controller keeps each control-plane configuration as an append-only snapshot in `control_plane_configurations`, which includes the Kubernetes version. Each control-plane node is pinned to the snapshot it was built from. A version upgrade appends a new snapshot, and so does any other change to the control-plane configuration. A change to a field listed in `ComputeControlPlaneRolloutChanges` (`controller/internal/db/control_plane_configurations.go`) replaces the pinned nodes. A node pool works the same way through node sets: each node set copies the pool's Kubernetes version and other node settings, a change to any of them creates a new node set, and each worker belongs to the node set it was built from. A node set is deleted once its last node is gone.

A separate control-plane isolation design, still in draft, replaces this. NVKE renders a configuration file for each control-plane host, and the node agent runs individual `kubeadm init phase` commands from it. Control-plane hosts then run a standalone kubelet that registers no `Node` and runs only their static pods, and workers start their kubelet without `kubeadm join`. Nothing is read back from the workload cluster. This design works with both; see [Rendering](#rendering).

## Problem

A platform operator who needs a gate that is off by default has no supported way to turn it on. One who needs to turn off an on-by-default gate can't do that either. On 1.36, NVKE's supported version, these beta gates are off by default:

| Gate | 1.36 | 1.37 |
|---|---|---|
| `MaxUnavailableStatefulSet` | Beta, off | Beta, on |
| `SELinuxMount` | Beta, off | GA, on |
| `SchedulerAsyncAPICalls` | Beta, off | Beta, off |
| `DRADeviceTaintRules` | Beta, off | GA, on |

## Goals

- Platform operators set gates on each control-plane component through `ControlPlaneClass`.
- The controller takes gates per component, as given, through its REST API, at creation and on update.
- Gates carry over through node replacement, scale-out and upgrades.
- A change to the gates reaches every control-plane host.

## Non-goals

- **Checking gates against the Kubernetes version.** The caller makes sure a gate is valid and works with the rest of the system. A check in the controller is [later work](#later-work).
- **Deciding which gates are allowed.** Which stages, or which gates, a platform offers is policy for the platform operator and the management engine, not the controller. The controller passes alpha gates through like any other.
- **kube-proxy and etcd gates.** NVKE doesn't pass gates to either.
- **Kubelet gates on control-plane hosts.** See [Control plane](#control-plane).
- **kube-apiserver `--runtime-config`.** See [Later work](#later-work).

## API

### Control plane

`CreateClusterControlPlaneConfig` and `UpdateClusterControlPlaneConfig` get `featureGates`, with one optional map per component. The names match the components in kubeadm's `ClusterConfiguration`:

```yaml
controlPlaneConfig:
  desiredCount: 3
  featureGates:
    apiServer:
      MaxUnavailableStatefulSet: true
    controllerManager:
      MaxUnavailableStatefulSet: true
    scheduler:
      SchedulerAsyncAPICalls: true
```

Each map goes only to its component. A feature that needs more than one component has to be set in each of them: most DRA features need all three. Kubernetes doesn't record which component reads which gate, so the controller can't fill the others in.

On update, `featureGates` replaces all three maps, so a caller sends the whole object. A map that is left out or empty sets no gates on that component. Leaving `featureGates` out of the update keeps the current maps.

`Cluster.controlPlaneConfig.featureGates` returns the stored maps.

The maps are stored in the control-plane configuration snapshot, next to the Kubernetes version, so every snapshot records the version and the gates together. `featureGates` is added to the fields in `ComputeControlPlaneRolloutChanges`, so a change replaces the control-plane nodes; see [Changing gates](#changing-gates).

The control plane has no kubelet map. Under control-plane isolation, a control-plane host runs a standalone kubelet that registers no `Node` and runs only that host's static pods. Kubelet gates such as `SELinuxMount` or the DRA gates act on workload pods, so on that kubelet they would do nothing, and a bad one could only stop it. NVKE still sets its own kubelet gates there; see [Gates NVKE sets](#gates-nvke-sets).

### ControlPlaneClass

`ControlPlaneClassConfig` gets `featureGates` with the same shape:

```yaml
apiVersion: internal.nvke.nvidia.com/v1
kind: ControlPlaneClass
spec:
  controlPlane:
    replicas: 3
    featureGates:
      apiServer:
        MaxUnavailableStatefulSet: true
      controllerManager:
        MaxUnavailableStatefulSet: true
```

`resolveNVKEControllerClass` copies it into the create request. `ensureCluster` compares it with the cluster's stored maps, as it does `infra` and `maxSurge`, and sends `featureGates` in the update when they differ. Every cluster that selects the class gets the change.

### Node pools

`NodePoolCreateConfig` gets `kubeletFeatureGates`, a map from gate name to `true` or `false`, which works the same way: passed through as given, stored on the pool and copied into each node set next to the Kubernetes version. A change creates a new node set (`NodePoolNeedsNewNodeSet` in `controller/internal/db/node_sets.go`), which replaces the pool's nodes.

The node pool side is still under review. Where the management engine takes a pool's gates from is open; see [Open questions](#open-questions).

## Validation

The controller doesn't check that a gate exists in the Kubernetes version, what stage it is at, whether it is locked, or whether its dependencies are on. A gate that fails any of these stops the component; see [When a gate is wrong](#when-a-gate-is-wrong).

The request schema checks the shape: each map is from a non-empty gate name to a boolean. That includes `AllAlpha` and `AllBeta`, which turn on a whole stage, including gates a later version adds.

### Gates NVKE sets

NVKE sets some gates itself, on the control-plane components and on every kubelet, including the standalone kubelet on control-plane hosts. Today that is only `ImageVolume`, on kube-apiserver and the kubelet, below 1.36. NVKE's value takes precedence: an operator value that agrees with it is merged, and one that contradicts it is rejected with a 400 naming the gate. This is the one check the controller makes, because NVKE's own components depend on those gates.

The check runs in the request handler, before the handler writes anything, against the version the request will run on.

## Rendering

The stored maps are the only source of gates. Each host gets its gates rendered from them for the Kubernetes version that host runs, merged with the gates NVKE sets for that version. Each rendered value is one list sorted by gate name, so golden-file output is stable.

Today's bootstrap and control-plane isolation both meet this. They differ only in where the rendered gates go.

### Control plane

Each map goes into the `extraArgs` of its component in the `ClusterConfiguration`, as one `feature-gates` entry: `apiServer`, `controllerManager` and `scheduler`. `extraArgs` is already built as a map and converted to the list form v1beta4 needs, so no conversion code changes. NVKE's `ImageVolume` already goes into `apiServer.extraArgs` (`controller/internal/kubeadmconfig/init.go`) and is merged with the operator's `apiServer` map. The gates don't go in `ClusterConfiguration.featureGates`, which holds kubeadm's own gates.

Each control-plane host renders from the snapshot it is pinned to, which holds that host's version and gates.

**Today.** The renderer adds the entries at `kubeadm init`, and kubeadm stores the configuration in `kube-system/kubeadm-config`. Control-plane nodes that join later, for scale-out, replacement or an upgrade, take their flags from that ConfigMap, not from a fresh render. The OIDC and etcd encryption rollouts write their settings into the same ConfigMap for the same reason. Upgrades and gate changes rewrite the entries there before any node is replaced; see [Upgrades](#upgrades) and [Changing gates](#changing-gates).

**Control-plane isolation.** Each control-plane host gets its own configuration file, rendered by NVKE and written by the node agent. `kubeadm init phase control-plane all` writes the static pod manifests from it. Every host's file is rendered from its pinned snapshot, so scale-out, replacement and upgraded hosts get the current gates with no ConfigMap step, and a host not yet replaced keeps its own.

Either way, the standalone or registered kubelet on a control-plane host gets only the kubelet gates NVKE sets for its version (today `imageVolumeKubeletFeatureGates` in `render.go`, which returns `ImageVolume` below 1.36). Today they reach it the same way as a worker's; see below.

### Node pool kubelets

A node's kubelet `featureGates` is the `kubeletFeatureGates` of its node set, merged with the kubelet gates NVKE sets for the node set's version. It is rendered from the database every time a node joins, so replacement, scale-out and upgraded nodes all get their node set's gates. The version is the node set's, not the cluster's: the join activity (`controller/internal/temporal/activities/kubeadm_join.go`) passes `cluster.KubernetesVersion` today, which is the control plane's version and can be up to three minors ahead of the pool.

**Today.** `kubeadm join` builds the kubelet configuration from `kubelet-config` and the patch files, not from the join file, so the gates go into a patch. A merge patch would only add keys: a gate already in `kubelet-config` would stay, whether a customer with cluster-admin put it there or `kubeadm init` did, like the `ImageVolume=true` of a cluster created before 1.36. So the gates go into a separate JSON patch, `/etc/kubernetes/patches/kubeletconfiguration1+json.yaml`, with one `add` operation on `/featureGates`. kubeadm applies it after `kubeletconfiguration0+merge.yaml`, and it replaces the whole map. NVKE writes it on every node, at `kubeadm init` and every `kubeadm join`, even when the map is empty, so an empty map clears what `kubelet-config` holds. NVKE's own kubelet gates move into this patch, and the copies in the init and join documents are removed so there is one source. `kubeadm upgrade apply` already passes `--patches` with the same directory, so it keeps the gates when it rewrites the kubelet configuration on a control-plane host.

**Control-plane isolation.** Workers start their kubelet without `kubeadm join`, from a `KubeletConfiguration` NVKE renders in full, and the gates go into it directly. This assumes that configuration is rendered from the worker's node set in the database, as the patch is today; see [Open questions](#open-questions).

## Upgrades

An upgrade carries the stored gates to the target version unchanged. For the control plane, the update handler already appends a snapshot with the target version, and the maps are copied into it. For a node pool, the node pool controller copies the pool's map into the new node set it creates for the target version. If the same request changes the gates, the new maps go into the new snapshot or node set instead.

The controller doesn't check the gates against the target version. A gate the target version has removed or locked to the other value stops the new nodes' components; see [When a gate is wrong](#when-a-gate-is-wrong). The caller changes the gates before the upgrade, or in the same request.

**Control plane, today.** The upgrade reconciler (`controller/internal/controllers/controlplane/k8s_upgrade.go`) already writes the target version into `kubeadm-config` before it starts replacing control-plane nodes. It does that through the `UpdateKubeadmConfigMap` workflow, which calls `EnsureKubernetesVersionInClusterConfiguration` in `controller/internal/kubeadmconfig/reconcile.go`. In the same step, it recomputes each component's `feature-gates` from the new snapshot and the gates NVKE sets on the target version, and replaces the old value rather than adding to it, so a pre-1.36 cluster's `ImageVolume=true` is dropped. It uses the same `reconcileClusterConfiguration` helper, which already handles both the v1beta3 and v1beta4 shapes. The rest of the upgrade is unchanged: new-version nodes replace old ones, then `kubeadm upgrade apply` upgrades the add-ons.

**Control plane, under isolation.** There is no ConfigMap to update. Each new-version host renders its configuration file from the new snapshot. A host still on the old version stays pinned to the old snapshot until it is replaced. If its file is rewritten before then, for example to update etcd membership, the rewrite renders from that snapshot too. Otherwise a phase re-run there would start old-version components with the new gates.

**Node pool.** Replacement nodes render their kubelet configuration from the new node set, under either bootstrap. Nodes still in the old node set keep its version and gates until they are replaced.

**Rollback.** NVKE doesn't support rolling back a Kubernetes version today. Because each snapshot and node set records the gates with the version, a later rollback design can restore both together.

## Changing gates

A control-plane update with different `featureGates` appends a snapshot, and because `featureGates` is a rollout field, the control-plane nodes are replaced one at a time, as for any other rollout change.

**Today.** Before the first node is replaced, the rollout rewrites each component's `feature-gates` in `kubeadm-config` from the new snapshot, with the same step the upgrade uses. Otherwise replacement nodes would join with the old gates.

**Control-plane isolation.** Each replacement host renders from the new snapshot. No ConfigMap step is needed.

A node pool update with different `kubeletFeatureGates` creates a new node set, and its nodes are replaced as for any other node set change.

A feature that spans the control plane and the kubelet needs the control plane changed first, so the API server serves it before kubelets use it. The controller doesn't order the two; the caller does.

## When a gate is wrong

A gate that the version doesn't know, that is locked to the other value, or whose dependencies are off stops its component from starting on the first node that gets it. At creation, that leaves the control plane half built. On an upgrade or a gate change, the first replacement node doesn't come up and the rollout stops there, with the remaining nodes still on the previous snapshot. The error appears in the component's log on the node.

The fix is to correct the gates with another update. How a stuck rollout picks up the corrected snapshot is the controller's existing rollout behavior, which this design doesn't change.

## Security considerations

Setting gates needs the same permission as creating or updating the cluster or node pool, and a gate affects only that cluster. Through the management engine, gates come from `ControlPlaneClass`, which the platform operator publishes and tenants only select, so tenants can't set gates.

Alpha gates are passed through. Whether to offer them is the platform operator's decision.

With today's bootstrap, the control-plane gates live in `kube-system/kubeadm-config` inside the workload cluster, where a customer with cluster-admin can edit them. A control-plane node that joins later would start with whatever is there. Control-plane isolation closes this: every host renders from NVKE's database, and nothing is read back from the cluster. The same customer can edit `kubelet-config` today, but the JSON patch replaces its whole `featureGates` map, so kubelet gates come only from the database under both. The rest of `kubelet-config` stays editable until isolation.

## Testing

- **Rendering:** golden-file tests for the init configuration, with a different map on each component, and for the kubelet JSON patch, for the v1beta3 and v1beta4 shapes, with and without NVKE's own gates. A test applies the patch over a kubelet configuration that holds a stale gate and checks the gate is gone. A control-plane host pinned to an older snapshot renders that snapshot's version and gates, and a worker renders its node set's version, not the cluster's. Control-plane isolation adds golden files for its per-host configuration file and worker kubelet configuration, which fail if its renderer leaves the gates out.
- **Handlers:** an operator value that contradicts a gate NVKE sets returns 400 and writes nothing. A create stores the maps in the first snapshot. An upgrade copies them into the new snapshot. An update that changes them appends a snapshot that `ComputeControlPlaneRolloutChanges` reports as a rollout.
- **`kubeadm-config`:** the upgrade and the gate-change rollout replace each component's `feature-gates`, and drop a value that the new snapshot no longer sets.
- **Management engine:** a class with gates produces a create request with them, and a change to the class produces one update with the new maps.
- **End to end:** on a local cluster through Tilt and CAPD, each component's flags appear on its static pod, and `featureGates` appears in each worker's kubelet configuration.

## Delivery

Each step is its own pull request, in this order:

1. Control-plane `featureGates` in the controller: create and update, storage in the snapshot, the rollout field, rendering for today's bootstrap, and the `kubeadm-config` step for upgrades and gate changes.
2. `ControlPlaneClass.featureGates` in the management engine, passed through on create and update.
3. Node pool `kubeletFeatureGates`, with rendering and a database migration for pools and node sets, once the node pool review settles where the gates come from.
4. The documentation and a demo script. See [Documentation](#documentation).

Steps 1 and 3 both change `openapi.yaml`, so they land one after the other.

Control-plane isolation is delivered separately. Whichever lands second carries the gates into the isolation renderer, and its golden files show they are there.

## Later work

These are outside this design. Each needs its own design before work starts.

- **Checking gates against the Kubernetes version.** If wrong gates turn out to be a problem in practice, the controller checks them in the request: that each gate exists in the version it will run on, isn't locked to the other value, and has its dependencies on. It belongs in the controller, not the management engine, because a gate that stops a component is a problem for every caller, including NVKE's own clusters. Kubernetes publishes the data per version in `test/compatibility_lifecycle/reference/versioned_feature_list.yaml`, and the registry in `k8s.io/kubernetes/pkg/features` records each gate's dependencies, so the check can be generated rather than maintained by hand. Upgrades would run the same check against the target version.
- **Gates NVKE requires.** NVKE may rely on unlocked, on-by-default gates, such as the DRA betas on 1.36, that an operator could turn off. Rejecting those needs a list of them, and the owners of NVKE's GPU stack to say what goes on it.
- **kube-apiserver `--runtime-config`.** Some gates also need an API version that is off by default. On 1.36, a `DeviceTaintRule` needs `DRADeviceTaintRules` and `resource.k8s.io/v1beta2=true`. On 1.37 it needs neither.

## Open questions

- **Where node pool gates come from in the management engine.** `NVKECluster` node pools have no class today, and the engine only updates a pool's node count. Whether gates go on the pool, on a new pool class or elsewhere is for the node pool review.
- **Control-plane isolation.** That design is still in draft, and this one assumes three things of it:
  - every render of a control-plane host's configuration file, including a rewrite after bootstrap, reads the snapshot that host is pinned to;
  - a worker's `KubeletConfiguration` is rendered by NVKE from the database for its node set on every join, not taken from a control-plane host's file;
  - an upgrade regenerates a host's static pods only from that host's file. Today `kubeadm upgrade apply` regenerates them from `kubeadm-config`, which isolation removes.

  Because the snapshots and node sets are the source of gates under both bootstraps, moving an existing cluster to isolation needs no extra step for its gates.

## Documentation

- A new page, `docs/workload-clusters/feature-gates.md`, for platform operators. It covers:
  - setting gates on `ControlPlaneClass` and on node pools;
  - that each control-plane component gets only its own map, and a short table of commonly used gates and the components that read them, without trying to list every gate;
  - setting a feature that needs both the control plane and the kubelet in both, control plane first;
  - that NVKE doesn't check gates against the version, what a wrong gate looks like, and checking gates before moving a class's clusters to a new Kubernetes version;
  - the gates NVKE sets, which can't be contradicted.

  Its example uses a beta gate that is off by default on 1.36.
- The [supported platforms](../overview/supported-platforms.md#kubernetes-feature-gates) page currently says NVKE sets no gates and has no field for them, and that NVKE enables no alpha gates. It changes to link the new page, and to say that gates other than the defaults are the platform operator's choice.
- A `demo.sh` that creates a cluster from a class with gates and a node pool with gates, and shows them on each control-plane component and a worker's kubelet. Where it lives is open for review, because the repository has no convention for demo scripts.
