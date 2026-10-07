# Backpressure

Collector pipelines use synchronous processors and batch data within exporter
queues. With `block_on_overflow: true`, full queues make incoming calls wait for
capacity, allowing downstream congestion to slow ingestion. The memory limiter
can refuse data under memory pressure, so receivers and upstream senders must
retry retryable failures. The Filelog receiver retries these failures until
recovery and drops errors marked permanent.

Each Collector exports its own telemetry through separate SDK buffering to its
[self-telemetry sidecar](self-telemetry.md). Logs and traces use bounded
asynchronous queues, and metrics use a periodic reader. The sidecar has its own
bounded queue and does not block when it is full. This keeps monitoring
congestion from blocking collection.
