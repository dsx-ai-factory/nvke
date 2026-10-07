# kube-state-metrics

The DaemonSet under `kube-state-metrics.daemonset` collects scheduled Pods on its node.
Configure central workloads under `kube-state-metrics.deployments`. Each central workload's `resource_types`
selects the Kubernetes resource types to collect and grants matching RBAC.
Set `track_unscheduled_pods: true` to collect Pods without a node assignment.
This is supported only for central workloads and requires `resource_types` to be exactly `[pods]`.
Resource names come from the [upstream resource list](https://github.com/kubernetes/kube-state-metrics/blob/v2.20.0/docs/README.md#exposed-metrics).

`mutatingadmissionpolicies` and `mutatingadmissionpolicybindings` are disabled by
default because KSM uses their `admissionregistration.k8s.io/v1` APIs, which older
clusters do not serve. Add them to `resource_types` only when the cluster serves both APIs.

`kube-state-metrics.namespaces` sets the namespace scope unless a workload supplies
its own `namespaces` list. RBAC follows the effective namespace selection.
An empty list means all namespaces. Empty strings are ignored.

Set `kube-state-metrics.metricLabelsAllowlist` to expose selected Kubernetes labels,
for example `["pods=[app.kubernetes.io/name]"]`. Additional Kubernetes labels are
disabled by default.

Secret metrics are opt-in because collection requires list/watch access to complete
Secret objects, including their contents. To enable them, set
`kube-state-metrics.deployments.configuration.resource_types` to `[configmaps, secrets]`.
Set that workload's `namespaces` list to limit access. An empty list grants access
across the cluster.

Shared `kubernetes` defaults can be overridden under each workload. `resources`,
`env` and `securityContext` configure the KSM container, `podSecurityContext`
configures Pod security, and remaining fields configure the Pod spec.
Set `serviceAccount` to use an existing account.

When overriding scheduling settings, ensure the node Collector runs on every node
where the KSM DaemonSet runs so it can scrape the local KSM endpoint.

Increase `shards` when a central workload exceeds its memory or scrape response
budget. Helm supplies `--shard` and `--total-shards`. Each shard runs as one
Deployment and briefly stops collection during replacement.

The [kube-state-metrics design](../designs/kube-state-metrics.md) compares the
approaches considered for scaling KSM collection.
