# CAPI Controllers

This package is for new controller-runtime or otherwise Kubernetes-native controllers in CWE.

These controllers should reconcile Kubernetes API state only. They should not depend on CWE-specific systems such as Temporal workflows or PostgreSQL state.

Most of the older controllers in `internal/controllers/` predate this split and are still CWE-specific. Many of them use Temporal and/or Postgres directly, and some are hybrid controllers that combine Kubernetes reconciliation with CWE-owned state.

Longer term, controllers in this package are expected to move into the Cluster API NKE providers (`bootstrap`, `controlplane`, or `capn`) once that code is ready to live outside CWE.
