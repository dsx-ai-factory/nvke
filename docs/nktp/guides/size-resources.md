# Size resources

Default budgets are per Pod. Configure collector resources under
`<component>.kubernetes.resources` and allocator resources under
`<component>.kubernetes.targetAllocator.resources`.

| Component | CPU / memory request | CPU / memory limit |
| --- | --- | --- |
| Node collector | 500m / 1 GiB | 2 / 2 GiB |
| Large-target collector | 500m / 2 GiB | 4 / 12 GiB |
| Metrics gateway | 250m / 512 MiB | 6 / 4 GiB |
| Logs and traces gateways | 250m / 512 MiB | 4 / 4 GiB |
| Cluster event reader | 250m / 256 MiB | 1 / 512 MiB |
| Watchdog | 100m / 256 MiB | 4 / 1 GiB |
| Node Target Allocator | 250m / 2 GiB | 2 / 4 GiB |
| Large-target and watchdog Target Allocators | 250m / 1 GiB | 1 / 2 GiB |
| Self-telemetry sidecar, watchdog and gateways | 20m / 64 MiB | 200m / 256 MiB |
| Self-telemetry sidecar, other Collectors | 10m / 32 MiB | 100m / 128 MiB |

Node collectors use a `768MiB` `GOMEMLIMIT`. Gateways use 1.5 GiB,
large-target collectors use 7 GiB and watchdog uses 192 MiB. Tune runtime memory
targets and queues together with container limits.
Node collector queues hold 64 MiB per signal, including in-flight payloads.

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
