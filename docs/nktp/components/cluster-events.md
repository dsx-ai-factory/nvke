# Cluster events

The cluster event reader sends Kubernetes Event logs to the logs gateway. Two
replicas share one watch through leader election. Its own metrics, logs and
traces go through its [sidecar](../architecture/self-telemetry.md) to watchdog.

Use `cluster_events.otelcol.config` for Collector configuration and
`cluster_events.kubernetes` for Kubernetes workload configuration. Retain
`GOMEMLIMIT`, `GOGC` and `POD_NAMESPACE` when replacing its environment list.
Set `receivers.k8s_events.namespaces` within the Collector configuration to limit
the watched namespaces. Events during a leadership handoff can be missed.

The chart creates the election Lease in the release namespace and restricts the
reader's Lease permissions to that name. Set `extensions.k8s_leader_elector.lease_name`
in the Collector configuration to change the name. Use a literal name so Helm can
create the matching Lease. If using another Lease namespace, provision the Lease
and its RBAC there separately.
