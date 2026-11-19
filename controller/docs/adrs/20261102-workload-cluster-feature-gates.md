# Operator-set Kubernetes feature gates on workload clusters

**Status:** proposed
**Date:** 2026-11-02

## Context

NVKE runs every Kubernetes component with the upstream default feature gates for its version, and neither the REST API nor the management engine has a field to change one ([supported platforms](../../../docs/overview/supported-platforms.md#kubernetes-feature-gates)). The only gate NVKE sets is `ImageVolume`, which `controller/internal/kubeadmconfig/render.go` turns on below 1.36, where it is off by default.

Platform operators need gates NVKE does not choose for them. On 1.36, `MaxUnavailableStatefulSet`, `SELinuxMount`, `SchedulerAsyncAPICalls` and `DRADeviceTaintRules` are beta and off by default.

For each Kubernetes minor version, a gate has a stage (alpha, beta, GA or deprecated) and a default. It may be locked to that default, and it may depend on other gates. Gates are removed a few releases after they reach GA or are deprecated. A component refuses to start when it gets a gate it doesn't know, a locked gate set to the other value, or an enabled gate whose dependencies are off. Components take gates as a `--feature-gates` flag, and NVKE already sets flags of its own on each control-plane component through kubeadm's `extraArgs`, including its security settings.

The management engine creates and updates clusters through the controller's REST API. An `NVKECluster` selects a `ControlPlaneClass`, which holds the platform operator's control-plane settings, and the engine sends the controller an update whenever the class and the cluster differ. Which settings a platform offers its tenants is decided there, not in the controller.

The controller keeps each control-plane configuration as an append-only snapshot that includes the Kubernetes version, and pins each control-plane node to its snapshot. A change to certain snapshot fields replaces the control-plane nodes. Node pools do the same through node sets. With today's bootstrap, control-plane nodes that join after creation take their component flags from the cluster's `kube-system/kubeadm-config` ConfigMap, and every joining node's kubelet configuration starts from the `kubelet-config` ConfigMap; a customer with cluster-admin can edit both. A separate control-plane isolation design, still in draft, replaces this with a configuration file NVKE renders for each host, and reads nothing back from the workload cluster.

## Decision

We will let platform operators set Kubernetes feature gates on `ControlPlaneClass`, with one map for each control-plane component: `apiServer`, `controllerManager` and `scheduler`. The management engine turns each map into a `feature-gates` flag and passes it to the controller in that component's `extraArgs`, on create and on update.

The controller's REST API takes `extraArgs` for each control-plane component, not feature gates as such. That keeps the controller's API flexible, with no field per feature: a later flag needs a field on the class and no controller change. There are no kubelet arguments for control-plane hosts: their kubelet runs only static pods, so kubelet gates would do nothing there. The [design](../../../docs/design/kubernetes-feature-gates.md) gives the fields and the rendering.

The controller takes arguments as given. It doesn't check that a flag or gate exists in the Kubernetes version, that a gate isn't locked, or that its dependencies are on, and it doesn't restrict stages: alpha gates pass through like any other. The caller makes sure the arguments are valid and work with the rest of the system. Which gates, flags or stages a platform offers is policy for the platform operator and the management engine.

The controller rejects an argument for a flag NVKE sets itself, because NVKE's security settings and its own rollouts depend on them. `feature-gates` is the exception: the controller merges the operator's gates with the ones NVKE sets, and rejects a gate that contradicts NVKE's value.

We will store the control-plane arguments in the control-plane configuration snapshot and each node pool's kubelet gates in its node sets, and render each host's arguments from its own snapshot or node set, for that version, treating them as the only source. A change to the arguments is a rollout change: it appends a snapshot and replaces the control-plane nodes, or creates a new node set. An upgrade carries the arguments to the target version unchanged. Today's bootstrap carries the control-plane arguments in `kubeadm-config`, and upgrades and argument changes rewrite them there before replacing any node. Kubelet gates go in a JSON patch that replaces whatever `featureGates` the `kubelet-config` ConfigMap holds. Under control-plane isolation each host renders from the snapshot it is pinned to.

Node pool kubelet gates follow the same model. Where the management engine takes them from is left to the node pool review.

Checking gates against the Kubernetes version, a list of gates NVKE requires, other flags on `ControlPlaneClass`, and volumes for flags that name a file are outside this decision.

## Consequences

The controller stays a pass-through for component arguments, like the rest of the kubeadm configuration it renders, and gains no per-version data to maintain. Platform operators can use any gate, alpha included, without an NVKE change, and a new flag needs only a class field.

Access to the controller's API is worth more: a caller can set any kube-apiserver flag NVKE doesn't set, including ones that change authentication or authorization. Through the management engine only the platform operator can, and only feature gates.

A wrong flag or gate is found when a component fails to start, not in the request. At creation, that leaves the control plane half built. On an upgrade or an argument change, the first replacement node doesn't come up and the rollout stops there. The error is visible only in the component's log on the node.

A gate or flag the target version has removed breaks the upgrade, and a release channel can move every cluster on a class to that version. The platform operator has to check a class's gates before its clusters move. If this turns out to be a problem in practice, a version check for gates goes in the controller, because a gate that stops a component affects every caller, including NVKE's own clusters. Kubernetes publishes the data it would need, so it can be generated rather than maintained by hand.

The controller keeps a list of the flags NVKE sets on each component, and it has to change whenever the renderer does. A test keeps the two in step.

Each component gets only its own arguments, so a feature that several components read has to be set in each, and a feature that spans the control plane and the kubelet in both. The controller can't fill them in, because Kubernetes doesn't record which component reads which gate.

A change to a class's gates replaces the control-plane nodes of every cluster that selects it.

The supported platforms page no longer holds: it says NVKE sets no gates and has no field for them.

## Options considered

- **Per-component `extraArgs` in the controller, gates on `ControlPlaneClass` (chosen):** keeps the controller's API generic and policy with the platform operator, and matches how kubeadm configures each component.
- **Per-component gate maps in the controller:** a narrower API that can't be used to override other flags, but each new flag needs its own controller field, and the controller would own a feature-specific shape that the management engine already gives the operator.
- **Per-cluster gates set by tenants:** gives tenants more control, but gates are a platform decision, and a tenant's bad gate breaks a control plane NVKE runs.
- **One gate map for all control-plane components:** saves setting a shared gate three times, but gives each component gates it doesn't read, and doesn't match kubeadm's per-component configuration.
- **Validate every gate in the controller against a catalog generated per Kubernetes version:** fails a bad gate in the request instead of on the node, and catches gates an upgrade would break. Left for later: it adds per-version data to every Kubernetes version NVKE adds, for a problem not yet seen.
- **Restrict stages, such as beta and GA only, in the controller:** a policy choice, which belongs with the platform operator.
- **Let operator arguments override flags NVKE sets:** the most flexible, but would let a caller weaken NVKE's security settings, and NVKE's own rollouts would overwrite the operator's value.
- **Kubelet arguments for control-plane hosts:** matches node pools, but under control-plane isolation that kubelet runs only static pods, so they could only break it.
