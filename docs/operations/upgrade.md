# Upgrading NKE

> **This page is a stub.** Upgrade tooling for the NKE site controller and its component stack is under active development. This page will be expanded once the upgrade workflows are available.

## What Will Be Covered Here

- Upgrading the NKE site controller itself (CWE, Temporal, CloudNative-PG, SPIRE)
- Rolling Kubernetes version upgrades for tenant clusters
- Upgrading CAPNICo and the infrastructure provider stack
- Upgrade ordering and dependencies between components
- Pre-upgrade checks and rollback procedures

## Tenant Cluster Kubernetes Upgrades

Kubernetes version upgrades for individual tenant clusters are available today via `nkectl`:

```bash
nkectl patch cluster <name> --patch 'controlPlaneKubernetesVersion: "1.34.0"'
```

CWE drives a rolling upgrade of the control plane nodes first, then worker nodes, using kubeadm under the hood through `nke-agent`. Monitor progress in the Temporal Web UI — see [Observability](observability.md).

## Site Controller Upgrades

Site controller upgrade tooling is **not yet available**. The site controller does not currently manage itself. Upgrading the NKE components (CWE Helm chart, operator versions) requires manual Helm operations and is not yet documented or supported through `nkectl`.

This is a known gap. Track progress at https://github.com/dsx-ai-factory/nke/issues.
