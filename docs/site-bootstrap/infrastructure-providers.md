# Infrastructure Providers

> **Note:** Installing a Cluster API infrastructure provider is a **post-bootstrap concern**. `nvkectl bootstrap site` installs Cluster API only into the *ephemeral* Kind cluster it creates and then destroys, so a freshly bootstrapped site cluster has no Cluster API and no provider on it. This page describes that second step.

A tenant cluster is created on a Cluster API **infrastructure provider**: a controller running on the site cluster that holds credentials for a backend and creates machines there. `nvkectl` never installs one. You do it once per site, before the first `nvkectl create cluster`.

This page covers [CAPNICo](https://github.com/dsx-ai-factory/cluster-api-provider-nico), the NICo provider, which the management plane accepts by default. [LaunchLayer](#launchlayer) is the other supported provider; a deployment accepts it only once you enable it. Both sections link out to the provider's own documentation rather than restating it.

## Prerequisites

| Requirement | Notes |
|---|---|
| A bootstrapped site cluster | From [Pre-Provisioned Nodes](pre-provisioned-nodes.md), with its kubeconfig |
| `kubectl` | Pointed at the site cluster |
| `clusterctl` | Matching the Cluster API version NVKE pins, `v1.13.6` |
| `helm` **v4 or later** | Helm 3 cannot install the CAPNICo chart; see [Troubleshooting](#troubleshooting) |
| Outbound network | `clusterctl init` fetches Cluster API and cert-manager from `github.com`. An air-gapped site needs a mirror |
| A NICo site | Already installed, machines enrolled, and someone who can issue you a token |

`nvkectl` is not required for the install itself, only for the verification step at the end.

## Order Matters

Install in this order:

1. Cluster API core and the kubeadm providers
2. The infrastructure providers: CAPNICo, [CAPLL](#launchlayer), or both
3. The management plane (the NVKE controller)

**The NVKE controller must come last.** It reads the `Machine`, `NicoMachine` and `LaunchLayerMachine` CRDs once, at process start, and silently skips four controllers when they are absent — the machine SPIRE entry controller, the NICo TPM SPIFFE controller, the NICo provider-ID controller, and the LaunchLayer SPIFFE controller. They are warnings, not failures, and there is no retry. If the controller is already running, restart it after the provider lands. Find it by the
chart's own label rather than a hardcoded name, since the deployment name
depends on the Helm release name your controller install used:

On an existing site, where the controller runs as `cloud-workflow-engine`, use `cloud-workflow-engine` for both the namespace and the label in the command below.

```bash
kubectl rollout restart deployment -n nvke-controller -l app.kubernetes.io/name=nvke-controller
```

## 1. Check the Control-Plane Taint Is Gone

Every node on a site cluster is a control-plane node, and neither cert-manager, the Cluster API managers nor the CAPNICo manager tolerates the control-plane taint. `nvkectl bootstrap site` removes it. Confirm that before anything that waits, or the waits in the next two steps hang until they time out with no useful message:

```bash
kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{": "}{.spec.taints}{"\n"}{end}'
```

No node should list `node-role.kubernetes.io/control-plane`. If one does, because the bootstrap reported that the removal failed, remove it by hand:

```bash
kubectl taint nodes --all node-role.kubernetes.io/control-plane:NoSchedule-
```

The site cluster also needs a default StorageClass before the controller's Postgres and SPIRE will schedule. That is part of installing the management plane, not this page.

## 2. Install Cluster API

Use NVKE's own pinned invocation. Do **not** use the unpinned `clusterctl init` line in CAPNICo's documentation: without explicit versions, `clusterctl` installs whatever is latest, which drifts from the version NVKE pins.

```bash
clusterctl init \
  --core cluster-api:v1.13.6 \
  --bootstrap kubeadm:v1.13.6 \
  --control-plane kubeadm:v1.13.6 \
  --wait-providers
```

This also installs cert-manager, which the providers depend on.

`v1.13.6` is the version NVKE pins, in `make/tool-versions.mk`. Keep them in step.

Running `clusterctl init` again later is supported and skips what is already present, so this is safe to re-run. Only a *conflicting* provider instance is an error.

## 3. Install CAPNICo

Two routes. **Pick one and stay with it.** Both create the same `capnico-system/controller-manager` Deployment and the same resource names, so mixing them does not give you two installs — it gives you two things claiming the same objects. Helm over a `clusterctl` install fails outright on ownership metadata; `clusterctl` over a Helm install patches Helm's resources and adds its own inventory entry, leaving the release and the cluster disagreeing about what is installed.

### Helm, from the published chart

The published chart is self-contained: it is stamped at publish time with the matching controller image, so it needs no `--set manager.image.*` and no pull secret. Both the chart and the image it references are public.

```bash
helm install capi-provider-nico \
  oci://ghcr.io/dsx-ai-factory/cluster-api-provider-nico/charts/capi-provider-nico \
  --version 0.0.45 \
  --namespace capnico-system --create-namespace \
  --wait
```

Pick a version from the [repository's releases](https://github.com/dsx-ai-factory/cluster-api-provider-nico/releases) rather than tracking latest, so a site can say which build it runs.

> **Do not pre-create the `capnico-system` namespace.** The chart creates it unconditionally. Creating it first makes Helm fail with `invalid ownership metadata`. This is why the credentials Secret comes *after* the install, in the next step.

### clusterctl, from the release assets

Every CAPNICo release publishes `infrastructure-components.yaml` and `metadata.yaml`, which is what `clusterctl` needs. Add the provider to your `clusterctl` configuration:

```yaml
providers:
  - name: nico
    url: https://github.com/dsx-ai-factory/cluster-api-provider-nico/releases/download/v0.0.45/infrastructure-components.yaml
    type: InfrastructureProvider
```

```bash
clusterctl init --infrastructure nico:v0.0.45 --config ./clusterctl.yaml --wait-providers
```

> **Pass `--config` explicitly.** `clusterctl` resolves its default configuration path through the OS convention, so `~/.config/cluster-api/clusterctl.yaml` is **not** read on macOS — it looks in `~/Library/Application Support/cluster-api/` instead. The failure is unhelpful: `failed to get configuration for the InfrastructureProvider with name nico`. Using `--config`, or `~/.cluster-api/clusterctl.yaml`, works on both.

## 4. Create the Credentials Secret

CAPNICo owns this. Follow **[3. Get a Machine-Usable Credential](https://github.com/dsx-ai-factory/cluster-api-provider-nico/blob/main/docs/getting-started.md#3-get-a-machine-usable-credential)** in its getting-started guide, which lists the exact keys.

Create it **after** the install, in the namespace the chart made:

```bash
kubectl create secret generic nico-credentials -n capnico-system \
  --from-literal=endpoint=<nico-api-base-url> \
  --from-literal=orgID=<your-org> \
  ...
```

`nvkectl` looks for a Secret named `nico-credentials` in `capnico-system` by default.

Before you create anything, read CAPNICo's **[2. Check the Instance Types Have Allocations](https://github.com/dsx-ai-factory/cluster-api-provider-nico/blob/main/docs/getting-started.md#2-check-the-instance-types-have-allocations)** section. An instance type with no allocation looks healthy and provisions nothing, and machines retry forever in `InstanceTypeUnavailable`. It is the single most common way a first cluster stalls.

## 5. Verify

Use `kubectl` here. `nvkectl get providers` cannot run yet: it needs a site registered with `nvkectl config set-site`, and that requires the controller's URL, which does not exist until you install the management plane in the next step.

```bash
# Cluster API core and the kubeadm providers
kubectl get pods -n capi-system
kubectl get providers -A

# CAPNICo
kubectl get deployment controller-manager -n capnico-system
kubectl get crd | grep nico
kubectl get secret nico-credentials -n capnico-system
```

The CAPNICo Deployment should be available, and `nicoclusters`, `nicoclustertemplates`, `nicomachines` and `nicomachinetemplates` should all be established.

Once the management plane is installed and you have run `nvkectl config set-site`, `nvkectl get providers` reports the same thing in one line, and `nico` should read `CONFIGURED`.

> **`CONFIGURED` is a narrower claim than it looks.** It means the provider's CRDs are established, its controller is running, and a credentials Secret is present. It does **not** check that Cluster API core is installed, that the credentials work, or that the backend is reachable. CAPNICo ships its own CRDs, so `nico` reads `CONFIGURED` even on a cluster with no Cluster API at all. Treat it as "the provider is installed", not "the site is ready".

The `CREDENTIALS` column of `nvkectl get providers` reports whether the credentials work, from the check CAPNICo runs on them every few minutes. It reads `HEALTHY` only when CAPNICo serves `NicoIdentity`, the site kubeconfig can list `NicoIdentity` objects, and one of them names the credentials Secret; otherwise it reads `UNSUPPORTED`, `UNREADABLE` or `NO-IDENTITY`. See [Infrastructure providers](../../cli/README.md#infrastructure-providers) for every value.

To prove the credentials actually work before committing hardware, follow CAPNICo's **[5. Prove the Credentials Before Creating Machines](https://github.com/dsx-ai-factory/cluster-api-provider-nico/blob/main/docs/getting-started.md#5-prove-the-credentials-before-creating-machines)** section, which applies a throwaway `NicoCluster` that validates the identity and provisions nothing.

## Troubleshooting

**Helm fails with `namespaces "capnico-system" already exists` or `invalid ownership metadata`.** Either you are on Helm 3, or you pre-created the namespace. The chart requires **Helm 4**; NVKE pins `v4.2.4`. No error message mentions the Helm version.

**`clusterctl init` or `helm --wait` hangs.** The control-plane taint is still on the nodes. See step 1.

**`clusterctl` cannot find the `nico` provider.** The configuration file is not where `clusterctl` looked. Pass `--config`. See step 3.

**A tenant cluster is accepted and nothing happens.** `nvkectl create cluster` validates the shape of the request, not that a provider exists to service it. The API returns success and nothing reconciles. Check `nvkectl get providers` first.

**Machines sit in `InstanceTypeUnavailable`.** The instance type has no NICo allocation. See step 4.

For provider-side conditions — `WaitingForBootstrapData`, `WaitingForIdentitySecret` and the rest — see CAPNICo's **[7. When It Stalls](https://github.com/dsx-ai-factory/cluster-api-provider-nico/blob/main/docs/getting-started.md#7-when-it-stalls)** section.

## LaunchLayer

[CAPLL](https://github.com/dsx-ai-factory/cluster-api-provider-launchlayer) runs tenant clusters on LaunchLayer compute. Install Cluster API as in step 2, then install CAPLL and point it at your LaunchLayer deployment as its [README](https://github.com/dsx-ai-factory/cluster-api-provider-launchlayer#against-a-real-launchlayer-deployment) describes: the manager needs `--base-url` and an `--identity-secret` holding its OAuth2 client credentials. NVKE supplies neither.

The management plane then has to accept LaunchLayer requests. Until it does, it rejects them with 400. Set these chart values:

| Value | Setting |
|---|---|
| `supportedInfraProviders` | Append `launchLayer` to the existing value rather than replacing it. The deploy overlay ships `"nico,infra"`, which becomes `"nico,infra,launchLayer"` |
| `capi.rbac.create` | `true`. It defaults to `false`, and the chart renders no CAPI or LaunchLayer RBAC without it |
| `capi.rbac.launchLayer` | `true`, the default. Grants the controller access to LaunchLayer's CRDs when `capi.rbac.create` is `true`. Keep it `true` for as long as CAPLL's CRDs are installed, even after removing `launchLayer` from `supportedInfraProviders`: the NVKE controller runs the LaunchLayer SPIFFE controller whenever the `LaunchLayerMachine` CRD exists, so existing clusters' new machines still join, and without the grant its watch is refused and the controller exits |
| `tenantAccess.enabled` | `true`, the default. The controller reaches a LaunchLayer cluster's API only through the kubeconfig Secret this publishes; with it off, LaunchLayer clusters stay `Provisioning` |
| `nodeAgent.spireAttestationMethod` | `dsxImds`, when `spiffe.enabled` is `true`. A LaunchLayer node attests to SPIRE only through DSX IMDS, so a SPIFFE-enabled deployment using any other method rejects LaunchLayer requests with 400 |
| `nodeAgent.dsxImds.operatorTenant` | Required with `dsxImds`; the chart refuses to render without it. Must match DSX Identity's `global.operatorTenant` |
| `nodeAgent.dsxImds.endpoint` | Required with `dsxImds`. The metadata endpoint the node attestor queries; the default, `http://169.254.169.254`, is the standard link-local address |

`nodeAgent.spireAttestationMethod` applies to every provider's new nodes, not only LaunchLayer's. NICo accepts any method, so on a site whose NICo nodes attest with TPM, setting `dsxImds` pins new NICo nodes to it as well, and they fail only when they attest. A TPM-attested NICo deployment cannot enable LaunchLayer this way.

DSX IMDS identifies bare-metal instances only. With SPIFFE on and `nodeAgent.externalMTLS.enabled` off, the management plane rejects with 400 any LaunchLayer node pool or control plane that sets `vmProfile`, since its nodes could never attest.

SPIRE needs the `dsx_imds` node attestor on both sides:

- **Server.** Add the `dsx_imds` node attestor plugin to the SPIRE server, with `jwks_url` and `issuer` naming DSX Identity's signing keys and issuer, and `audience` set to `spiffe://<trust-domain>/spire/server`.
- **Nodes.** The node image must install the agent plugin at `/usr/local/lib/spire-agent/dsx-imds-node-attestor-agent` and supply its checksum as `DSX_IMDS_PLUGIN_CHECKSUM` in `/etc/spire-agent/spire-agent.env`, or set `nodeAgent.dsxImds.pluginChecksum`. The controller writes the agent's attestor configuration itself.

The ordering rule above applies here too: the NVKE controller checks for the `LaunchLayerMachine` CRD once, at process start, and skips the LaunchLayer SPIFFE controller when it is absent. Install CAPLL before the controller, or restart the controller after it.

## What This Page Does Not Cover

- **The management plane (NVKE controller) install.** Separate step, and it must come after this one.
- **`cluster-api-provider-provisioned`.** It ships in this repository but is not a tenant-cluster infrastructure provider and is not installed on a site cluster.
- **Installing the GPU runtime on a tenant cluster.** Once a tenant cluster exists, drivers, operators and system configuration are installed with [NVIDIA AI Cluster Runtime](https://github.com/NVIDIA/aicr), a separate project.

## Next Steps

- [Deploy Repository](deploy-repository.md) — the ArgoCD application structure for the NVKE services stack
- [CAPNICo: Path C, Production Against an Existing NICo Site](https://github.com/dsx-ai-factory/cluster-api-provider-nico/blob/main/docs/getting-started.md#path-c-production-against-an-existing-nico-site) — the provider's own guide for production sites
- [Your first cluster](../first-cluster.md) — creating a tenant cluster once a provider is configured
