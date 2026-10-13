# Tenant Cluster Kubernetes Upgrades

Kubernetes version upgrades for individual tenant clusters are available via `nvkectl`:

```bash
nvkectl patch cluster <name> --patch 'controlPlaneKubernetesVersion: "1.34.0"'
```

This drives a rolling upgrade of the **control plane nodes** using kubeadm through `nvke-agent`. Worker node Kubernetes version upgrades are not yet available through this path. Monitor control plane upgrade progress in the Temporal Web UI — see [Observability](observability.md).

## Site Controller Upgrades

Site controller upgrade tooling is **not yet available**. The site controller does not currently manage itself. Upgrading the NVKE components (controller Helm chart, operator versions) requires manual Helm operations and is not yet documented or supported through `nvkectl`.

This is a known gap. Track progress at https://github.com/dsx-ai-factory/nvke/issues.
