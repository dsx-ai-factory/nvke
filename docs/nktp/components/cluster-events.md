# Cluster events

The cluster event reader sends Kubernetes Event logs to the logs gateway. Two
replicas share one watch through leader election. Its own metrics, logs and
traces go through its [sidecar](../architecture/self-telemetry.md) to watchdog.

All Event logs receive the NVKE telemetry pipeline (NKTP) resource attribute
`nvidia.telemetry.class: k8s.event`, identifying their category independently of
the reporting controller or service-name fallback.

Event logs use the reporting controller as `service.name`, falling back to
`kube-apiserver` when it is missing or empty. The receiver exposes both Kubernetes
`reportingController` and core API `reportingComponent` as
`k8s.event.reporting_controller`; it does not expose legacy `source.component`.
Loki exposes the Event service name as `service_name`. The reader's own telemetry
continues to use `nktp-cluster-events`.

The fallback can falsely group Events from unidentified producers with `kube-apiserver`
telemetry. To choose another fallback, override the statements below, replacing
`unknown_service` with any desired name, such as `k8s-events`. Helm replaces the
whole statements list, so retain the classification and controller statements.
Remove the final statement to leave the service name unset when the controller
is unavailable.

```yaml
cluster_events:
  otelcol:
    config:
      processors:
        transform/event_service:
          log_statements:
            - context: log
              statements:
                - set(resource.attributes["nvidia.telemetry.class"], "k8s.event")
                - set(resource.attributes["service.name"], attributes["k8s.event.reporting_controller"]) where (resource.attributes["service.name"] == nil or resource.attributes["service.name"] == "") and attributes["k8s.event.reporting_controller"] != nil and attributes["k8s.event.reporting_controller"] != ""
                - set(resource.attributes["service.name"], "unknown_service") where resource.attributes["service.name"] == nil or resource.attributes["service.name"] == ""
```

Use `cluster_events.otelcol.config` for Collector configuration and
`cluster_events.kubernetes` for Kubernetes workload configuration. Retain
`GOMEMLIMIT`, `GOGC` and `POD_NAMESPACE` when replacing its environment list.
Set `receivers.k8s_events.namespaces` within the Collector configuration to limit
the watched namespaces. Events during a leadership handoff can be missed.
Collector Pods inherit `priorityClass.name`; override
`cluster_events.kubernetes.priorityClassName` to use another class or `""` to opt out.

The chart creates the election Lease in the release namespace and restricts the
reader's Lease permissions to that name. Set `extensions.k8s_leader_elector.lease_name`
in the Collector configuration to change the name. Use a literal name so Helm can
create the matching Lease. If using another Lease namespace, provision the Lease
and its RBAC there separately.

Size for the active reader rather than averaging active and standby replicas.
