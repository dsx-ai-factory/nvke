# Design Principles

## Collector configurations are monolithic

Each OpenTelemetry Collector configuration in `values.yaml` is treated as a
monolithic value. The chart renders it exactly as written. Templates do not add,
remove, merge, or change anything inside it. No other value in `values.yaml` can
change it.

This guarantee ensures that NVKE telemetry pipeline (NKTP) does not limit a cluster operator's ability to
configure the Collectors for their needs.

The OpenTelemetry Operator still does its normal work when it creates each
Collector. For example, when a Target Allocator is enabled, the Operator
connects the `prometheus` receiver to it, as it does in any installation.
