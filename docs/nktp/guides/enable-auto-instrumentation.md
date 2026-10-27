# Enable auto-instrumentation

The OpenTelemetry Operator that NVKE telemetry pipeline (NKTP) installs can inject an OpenTelemetry agent
into Java, Node.js, Python, and .NET containers when a Pod is created. The
agent produces traces and metrics for the service without changes to its code
or image.

An `Instrumentation` resource configures the agents, including where they send
telemetry. Service owners opt in by annotating a Pod template with the
`Instrumentation` to use. See
[Use auto-instrumentation](send-telemetry.md#use-auto-instrumentation).

## Provide an Instrumentation

Set these values to create a shared `Instrumentation` named `default` in the
release namespace. In the endpoint, replace only the namespace after the dot
(`.nktp`) with the release namespace. Keep the `nktp-daemonset-collector` prefix.

```yaml
opentelemetry-kube-stack:
  instrumentation:
    enabled: true
    name: default
    exporter:
      endpoint: http://nktp-daemonset-collector.nktp:4318
    nodejs:
      env:
        - name: OTEL_EXPORTER_OTLP_ENDPOINT
          value: http://nktp-daemonset-collector.nktp:4317
```

Service owners reference it as `nktp/default`. Each agent sends to the node
collector on its own node. The Java, Python, and .NET agents use OTLP over HTTP
on port 4318. The Node.js agent uses OTLP over gRPC, so its endpoint is port
4317.

The other values under `opentelemetry-kube-stack.instrumentation` set the rest
of the `Instrumentation`, such as the sampler, propagators, and agent images.
See
[Injecting auto-instrumentation](https://opentelemetry.io/docs/platforms/kubernetes/operator/automatic/)
for these settings.
