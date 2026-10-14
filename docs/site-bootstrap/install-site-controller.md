# Install the Site Controller

`nvkectl bootstrap site` leaves a plain Kubernetes cluster. This page installs the management plane onto it: Postgres, Temporal, SPIRE, [CAPNICo](https://github.com/dsx-ai-factory/cluster-api-provider-nico), NKTP and the NVKE controller.

The install takes three inputs.

| Input | Says |
|---|---|
| The site spec | What is specific to this site. The same file `nvkectl bootstrap site` read, with its `controllerStack` section filled in. See [Site Spec](../../cli/docs/sitespec.md) |
| The release description | Which published artifacts to use, each by OCI reference and digest: the `nvkr.dev/release-description/v1` JSON that `nvkr release export` writes. Optional. See [Release](#release) |
| The deploy path, [`deploy/nico/`](https://github.com/dsx-ai-factory/nvke/tree/main/deploy/nico) | What to install and in which order. See [Deploy Path](deploy-repository.md) |

One command does the whole install:

```bash
deploy/nvke-deploy.sh install --sitespec sitespec.yaml --kubeconfig site.kubeconfig \
  --release release.json --site-releases releases/ --api-client-cidrs "10.0.0.0/8"
```

It runs six steps in order and stops at the first failure, with diagnostics under `$XDG_STATE_HOME/nvke/diagnostics/`. Every step is safe to repeat: fix the cause and run the same command again. `--dry-run` prints every kubectl, helm and clusterctl argv and changes nothing in the cluster. The [deploy README](https://github.com/dsx-ai-factory/nvke/blob/main/deploy/README.md) lists every flag and variable.

## Release

Verify the release, then export its description and pass that file as `--release`:

```bash
nvkr release verify --reference <repo@digest> --lane pipeline-attested --scope artifact-publication \
  --identity <signer identity regexp> --issuer <OIDC issuer> --cosign <cosign> --cosign-digest <sha256>
nvkr release export --reference <repo@digest> > release.json
```

`verify` checks the artifacts, signatures and attestations under your policy and exits non-zero when they fail. `export` writes the description: every output by name, kind, reference and digest, with its platform and chart version. It verifies nothing, so run it only on a reference `verify` accepted; the install prints the reference it found. Each item of the deploy path names the artifacts it uses in its `release.yaml`; the charts the release carries are installed from it by digest, the images the items run are pinned by digest, and a chart the release lacks comes from the checkout or its public repository. The release decides the version of a chart it carries; the pin in the deploy path decides for a chart the release lacks.

For a local build, add `--nvkr-run-dir .nvkr/runs/<attempt>`: charts and images come out of the Run's OCI layouts, and the images are imported into the node's containerd. No registry is involved.

Without `--release`, the charts come from the checkout and the public repositories, and the site spec names the controller and profiles images under `controllerStack.images`.

## Before You Begin

| Requirement | Notes |
|---|---|
| The site cluster kubeconfig | Written by `nvkectl bootstrap site`; `--kubeconfig` |
| The site spec, filled in | `nvkectl validate sitespec -f sitespec.yaml` must pass. `spire.trustDomain` is as it was for the bootstrap: set once before it, or unset, and then both commands use `nvkectl-bootstrap.nke.local`; `controllerStack.database.passwordFile` names a file the install creates with a generated password when it does not exist |
| An odd node count | One, three or five nodes, all control-plane |
| A full checkout of this repository | The controller and NKTP charts are installed from the tree |
| `nvkectl` | `make -C cli build`; `NVKECTL` names it, else `PATH`. It reads the deploy path and renders it |
| `helm` `v4.2.4`, `kubectl`, `clusterctl`, `python3` | The versions `make/tool-versions.mk` pins; `clusterctl` matches `CAPI_VERSION`. Helm 3 cannot install these charts. The script's item parser and template renderer are Python |
| Egress | The registries the release names, reachable from the nodes; the images the charts, the CAPNICo manifest and the Cluster API providers run; `github.com` for `clusterctl init`; `kubernetes-sigs.github.io`, `open-telemetry.github.io` and `prometheus-community.github.io` for the NKTP chart dependencies. A local Run needs none of the registries |

## What the install does

1. **Checks readiness.** The tools and the `helm` pin, the checkout, the site spec, the release description, the run directory, the cluster API, the node count and architecture, and egress. A failed check names the fix. Nothing is changed.
2. **Prepares the cluster.** Removes the control-plane `NoSchedule` taint when the cluster has no worker nodes. Installs rancher local-path-provisioner as the default StorageClass when none is default; a site with its own CSI driver sets `SKIP_STORAGECLASS=1` and annotates its own class `storageclass.kubernetes.io/is-default-class=true` instead.
3. **Installs Cluster API.** `clusterctl init` with the core and kubeadm providers at `CAPI_VERSION` from `make/tool-versions.mk`. cert-manager comes with it. A Cluster API of another version is an error: `clusterctl init` does not upgrade. See [Upgrade](../operations/upgrade.md).
4. **Installs the site controller.** Renders `deploy/nico` for the site and against the release into a private scratch directory, applies it item by item, and removes the scratch directory. Applies the site's `MachineImage`, `ClusterRelease` and `NodePoolRelease` resources from `--site-releases`. Waits for CAPNICo and the controller and asks the controller's `/readyz` for its database and Temporal.
5. **Secures and registers.** Patches the Cluster API controllers with the SPIRE exec-auth plugin (`controller/hack/apply-capi-spiffe-exec-auth.sh`), registers the site with `nvkectl config set-site`, and, with `--api-client-cidrs`, applies a NetworkPolicy that admits those networks to the controller API. The chart admits the API from pods in the cluster only, so a client off the nodes needs its network listed. `clusterctl upgrade` drops the exec-auth patch; rerun the install after it.
6. **Verifies.** Calls `/api/version` from the machine running the install, when `--api-client-cidrs` admits it.

A failed helm release is uninstalled before the rerun:

```bash
helm --kubeconfig site.kubeconfig uninstall <release> -n <namespace>
deploy/nvke-deploy.sh install ...
```

## Remove

Uninstall in reverse item order, then remove Cluster API:

```bash
helm uninstall nke-controller -n nke-controller
helm uninstall spire -n spire-server
helm uninstall temporal -n temporal
helm uninstall spire-crds -n spire-server
helm uninstall postgres -n postgres
helm uninstall nktp -n nktp
kubectl delete -f deploy/nico/capnico/capnico.yaml
clusterctl delete --all
```

Helm leaves PersistentVolumeClaims behind. Delete the `postgres` and `spire-server` PVCs yourself when the data is no longer needed.

## Upgrade

Verify and export the new release, then run the install again with the new description:

```bash
deploy/nvke-deploy.sh install --sitespec sitespec.yaml --kubeconfig site.kubeconfig --release new-release.json
```

The steps before the stack find nothing to do; the stack is upgraded in place. [Tenant Cluster Kubernetes Upgrades](../operations/upgrade.md) covers upgrades of tenant clusters.

## Extend to GitOps

The scripted install above is the baseline. Every item in `deploy/nico/nvke-deploy.yaml` is a Helm chart plus values files, or plain YAML. A GitOps tool can deliver the same items without changing the deploy path.

- **Source.** The clone is a valid git source. The controller and NKTP charts are in the tree, at `controller/deploy/charts/controller` and `nktp/deploy/charts/nktp`. Postgres, SPIRE CRDs, Temporal and SPIRE come from the references in the release. An OCI registry is a valid Helm source, so the same references serve Argo CD.
- **Order.** The item list order maps to sync waves, one wave per item.
- **The Certificate.** The `spire` item declares a `requires:` on the `oidc-provider-upstream-ca` Certificate. In GitOps that is a wave boundary or a health check on the Certificate, not a `kubectl wait`.
- **Secrets.** The `nvke-base-config` item renders two Secrets: `nico-credentials` and `dgxc-tenant-credentials`. Under GitOps those come from the operator's secret store. The namespaces and the Certificate chain in the same item are plain YAML and stay in git.
- **Imperative steps.** `clusterctl init` and `controller/hack/apply-capi-spiffe-exec-auth.sh` stay imperative, outside GitOps. The other steps do not change.

The per-site values the render writes belong in your own repository or your secret store. [Deploy Path](deploy-repository.md) describes the `argocd` item type for a deploy path that carries Argo CD Applications.

## Next Steps

- [Your first cluster](../first-cluster.md#7-create-a-tenant-cluster) — create a tenant cluster on the registered site
- [Infrastructure Providers](infrastructure-providers.md) — prove the NICo credentials before committing hardware
