# Shared NVKE schema chart

**Status:** proposed
**Date:** 2026-11-06

## Context

The NVKE controller consumes MachineImage, ClusterRelease and NodePoolRelease,
whose Go types are defined in Management Engine. Cluster Service uses those
catalog definitions and additional request/configuration types. Packaging their
schemas independently in both application charts duplicates generated files and
makes overlapping installation ownership likely.

The CLI embeds chart sources in a separate module so it can install components
without a repository checkout. Kustomize and schema generators also consume these
same API definitions.

## Decision

We will generate one canonical set of catalog schemas inside a shared `nvke-crds` chart
beside the existing embedded charts. Both Helm and Kustomize will consume those
files. The schema chart will run no controller workloads.

We will preserve the existing controller chart's three-schema default through a
conditional local dependency. Operators can reuse that owner or use one dedicated
catalog release per cluster and disable the application dependencies. Cluster
Service-specific schemas remain with Management Engine. Consumers will not
automatically claim schemas already managed by another installer.

We will manage schema updates as retained Helm resources, with compatible-version
selection and explicit ownership transfer. Existing records must survive removal
of a consumer or schema chart.

## Consequences

Schema changes have one generated source location. The CLI can embed the same
schemas without tracked copies, and Helm packages carry their pinned dependency.
Kustomize and generator fixtures must use the canonical location.

Independent ownership remains a deployment decision. Moving an existing owner
requires an explicit migration; defining a shared chart does not transfer ownership.
Future schema changes require a chart version and dependency update. Compatibility
and data migrations remain the operator's responsibility.

## Options considered

- **Shared chart with canonical generated files (chosen):** supports independent
  installation and application reuse while preserving the current controller path.
- **Committed bundle per consumer:** keeps source charts self-contained but duplicates
  schema files and makes ownership overlap easier.
- **Build-only bundles per consumer:** removes source copies but still ties schema
  ownership to application packaging and requires dependency assembly for every consumer.
