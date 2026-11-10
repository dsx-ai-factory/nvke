# Operator-set Kubernetes feature gates on workload clusters

**Status:** proposed
**Date:** 2026-11-02

## Context

NVKE runs every Kubernetes component with the upstream default feature gates for its version, and the API has no field to change one ([supported platforms](../../../docs/overview/supported-platforms.md#kubernetes-feature-gates)). The only gate NVKE sets is `ImageVolume`, which `controller/internal/kubeadmconfig/render.go` turns on below 1.36, where it is off by default.

Operators need gates NVKE does not choose for them. On 1.36, `MaxUnavailableStatefulSet`, `SELinuxMount`, `SchedulerAsyncAPICalls` and `DRADeviceTaintRules` are beta and off by default. Three of them are on by default in 1.37.

For each Kubernetes minor version, a gate has a stage (alpha, beta, GA or deprecated) and a default. It may be locked to that default, and it may depend on other gates. Gates are removed a few releases after they reach GA or are deprecated. A component refuses to start when it gets a gate it doesn't know, a locked gate set to the other value, or an enabled gate whose dependencies are off. `render.go` already guards `ImageVolume` for this reason: a kube-apiserver that does not start leaves the cluster half built, and the error is visible only on the node.

Every component reads its gates from one registry in `k8s.io/kubernetes/pkg/features`, which records each gate's stage, default and lock version by version, and the dependencies between gates. Kubernetes also publishes the version history as `test/compatibility_lifecycle/reference/versioned_feature_list.yaml`.

Today, control-plane nodes that join after creation, including the new nodes of an upgrade, take their configuration from the cluster's `kube-system/kubeadm-config` ConfigMap. The upgrade writes the target version into that ConfigMap before it replaces any node. A gate stored there is carried to the new version unless NVKE removes it, and a customer with cluster-admin can edit it. Every joining node's kubelet configuration likewise starts from the `kubelet-config` ConfigMap, with NVKE's patch files merged over it. The controller already keeps each control-plane configuration as an append-only snapshot that includes the Kubernetes version, and pins each control-plane node to its snapshot. Node pools do the same through node sets, which are deleted once their last node is gone. A separate control-plane isolation design, still in draft, replaces `kubeadm init` and `kubeadm join` with `kubeadm init phase` commands run from a file NVKE renders for each host. Control-plane hosts then run a standalone kubelet that registers no `Node` and runs only their static pods, and nothing is read back from the workload cluster.

## Decision

We will let operators set Kubernetes feature gates on workload clusters at creation: one map applied to `kube-apiserver`, `kube-controller-manager` and `kube-scheduler`, and one map for the kubelet in each node pool. There is no kubelet map for control-plane hosts: their kubelet runs only static pods, so kubelet gates would do nothing there. The [design](../../../docs/design/kubernetes-feature-gates.md) gives the fields, the validation and the upgrade handling.

We will accept only beta and GA gates. A gate must exist in the Kubernetes version it will run on and must not be locked to the other value, and every gate that ends up on must have its dependencies on. Operators may turn off an on-by-default beta gate under these rules. We will reject alpha and deprecated-stage gates, and `AllAlpha` and `AllBeta`, which switch on a whole stage rather than one feature. Allowing alpha gates is a separate decision.

We will validate against a catalog generated from Kubernetes' own gate registry: one entry per Kubernetes minor version that clusters can be created on or upgraded to, committed to the controller and regenerated whenever such a version is added.

We will validate in the API request itself, before NVKE writes anything: at cluster creation, node pool creation, and upgrade. An invalid gate gets a 400 that names the gate and the reason. There is no separate notice or warning mechanism.

On upgrade, we will recheck the stored gates against the target version, next to the existing version-skew checks. A gate the target version still accepts is kept, including one that has become deprecated. A gate the target version has removed or locked is dropped if the target version uses the operator's value anyway. Otherwise, and when the stored gates fail the dependency rule, the upgrade gets a 400 naming every stored gate involved. It keeps failing until the operator resends it with gates listed in `acknowledgedFeatureGates`. NVKE drops those gates and then reruns every rule on the result. The map an upgrade replaced stays recorded with the version it upgraded from, so a later rollback can restore both: the control plane's previous snapshot already holds them, and a node pool upgrade records them, because the old node set doesn't outlive the upgrade.

Where NVKE sets a gate itself, NVKE's value wins, and an operator value that contradicts it is rejected. NVKE keeps a short, hand-maintained list of unlocked gates it requires, and setting one to `false` is rejected the same way.

We will store the control-plane map in the control-plane configuration snapshot and each pool's map in its node sets, and render each host's gates from its own snapshot or node set, for that version, treating those maps as the only source. Today's bootstrap carries the control-plane gates in `kubeadm-config`, and the upgrade rewrites them there. Kubelet gates go in a JSON patch that replaces whatever `featureGates` the `kubelet-config` ConfigMap holds. Under control-plane isolation each host renders from the snapshot it is pinned to.

Until upgrades recheck stored gates, a version change on a cluster or node pool with stored gates is rejected.

Changing gates on a running cluster, kube-apiserver `--runtime-config`, and gate fields in the management engine are outside this decision.

## Consequences

An invalid gate fails the request that introduced it, and the operator sees why in the response, instead of a component failing to start on a node. The management engine creates clusters and node pools through the same REST API, so the check covers it too.

Operators can use any beta or GA gate without an NVKE change. NVKE does not vet what a gate does: the catalog shows a gate is valid for the version, not that the feature works with NVKE's nodes, drivers or images. The catalog can't say which component reads a gate either. A gate in the wrong map is accepted and ignored, and a feature that needs both the control plane and the kubelet has to be set in both maps.

The list of required gates is the one hand-maintained part, and it has to be reviewed when a Kubernetes version is added.

The controller carries gate data for every supported Kubernetes version, and adding a version includes regenerating it. A version with no catalog entry has no valid gates, so a request with gates for it fails rather than passing unchecked.

Existing clusters can get kubelet gates only through a new node pool, and no control-plane gates, until changing gates on a running cluster lands. Storing the maps in snapshots and node sets leaves a direct path to that: a gate change becomes one more field that replaces nodes.

An upgrade can be refused because of a gate even when the version skew is valid. The operator makes that call before anything starts, and nothing is left to follow up afterwards.

The supported platforms page no longer holds: it says NVKE sets no gates and has no field for them.

## Options considered

- **Generated per-version catalog, checked in the request (chosen):** applies Kubernetes' own rules to every gate and fails before anything is built.
- **Pass gates through unchecked:** the smallest change, but a bad gate stops a component partway through provisioning or an upgrade, where the operator can't see why.
- **Hand-maintained list of vetted gates:** easy to review, but every new gate needs an NVKE release, and the list drifts from upstream.
- **Allow alpha gates too:** gives operators more, but alpha gates can change or disappear in any release and fall outside NVKE's support statement, so that is left to its own decision.
- **Warn about invalid gates after an upgrade:** lets the upgrade proceed, but changes the cluster's behavior before the operator has agreed to it.
- **A kubelet map for control-plane hosts:** matches node pools, but under control-plane isolation that kubelet runs only static pods, so the map could only break it.
- **Record which component reads each gate:** would let validation reject a gate in the wrong map, but Kubernetes doesn't record it, so every gate would be classified by hand for every release.
