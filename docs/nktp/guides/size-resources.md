# Size resources

Default budgets are per Pod. Configure collector resources under
`<component>.kubernetes.resources` and allocator resources under
`<component>.kubernetes.targetAllocator.resources`.

| Component | CPU / memory request | CPU / memory limit |
| --- | --- | --- |
| Node collector | 1 / 2 GiB | 2 / 4 GiB |
| Large-target collector | 2 / 8 GiB | 4 / 16 GiB |
| Metrics, logs and traces gateways | 200m / 512 MiB | 4 / 8 GiB |
| Cluster event reader | 1 / 512 MiB | 4 / 1 GiB |
| Watchdog | 500m / 512 MiB | 4 / 1 GiB |
| Node Target Allocator | 250m / 2 GiB | 2 / 4 GiB |
| Large-target and watchdog Target Allocators | 250m / 1 GiB | 1 / 2 GiB |
| Self-telemetry sidecar, watchdog and gateways | 20m / 64 MiB | 200m / 256 MiB |
| Self-telemetry sidecar, other Collectors | 10m / 32 MiB | 100m / 128 MiB |

Node collectors use a `3276MiB` `GOMEMLIMIT`. Gateways use 7 GiB,
large-target collectors use 8 GiB and watchdog uses 768 MiB. Tune runtime memory
targets and queues together with container limits.
The node metrics queue holds 32 MiB to admit larger scrapes before batching.

HPA targets are relative to requests. Increasing limits does not raise the
scaling threshold. Reserve node capacity for growth and failure recovery, and
start enough scraper replicas to absorb large initial loads. These budgets do
not guarantee a particular series count. Validate allocator discovery and
reassignment under load, especially after restarts.

See [Large-target metrics](../components/large-target-metrics.md#sizing-large-scrapes)
for sizing the pool for large scrape responses, and
[Backpressure](../architecture/backpressure.md) for how queues and the memory limiter
behave when a component is saturated. See
[Self-telemetry sidecars](../architecture/self-telemetry.md#sizing) for how the
sidecar sizes were chosen.
