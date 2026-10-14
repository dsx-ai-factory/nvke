# Install the Site Controller

`nvkectl bootstrap site` leaves a plain Kubernetes cluster. This page installs the management plane onto it: Postgres, Temporal, SPIRE, [CAPNICo](https://github.com/dsx-ai-factory/cluster-api-provider-nico), NKTP and the NVKE controller.

The install takes three inputs.

| Input | Says |
|---|---|
| The site spec | What is specific to this site. The same file `nvkectl bootstrap site` read, with its `controllerStack` section filled in. See [Site Spec](../../cli/docs/sitespec.md) |
| The release description | Which published artifacts to use, each by OCI reference and digest: the `nvkr.dev/release-description/v1` JSON that `nvkr release export` writes. Optional. See [Release](#release) |
| The deploy path, [`deploy/nico/`](https://github.com/dsx-ai-factory/nvke/tree/main/deploy/nico) | What to install and in which order. See [Deploy Path](deploy-repository.md) |

## Release

Verify the release under your policy, then export its description and pass the file as `--release`. NVKR's release quickstart covers [verify](https://github.com/dsx-ai-factory/nvkr/blob/main/docs/release-quickstart.md#verify-against-an-accepted-policy) and [export](https://github.com/dsx-ai-factory/nvkr/blob/main/docs/release-quickstart.md#export-the-complete-description).

```bash
nvkr release export --reference <repo@digest> > release.json
```

`export` verifies nothing, so run it only on a reference `verify` accepted. With the description, the charts the release carries are installed by digest and the images are pinned by digest; a chart the release lacks comes from the checkout or its public repository. Without `--release`, every chart comes from the checkout and the public repositories, and the site spec names the controller and profiles images under `controllerStack.images`. For a local build, add `--nvkr-run-dir .nvkr/runs/<attempt>`: charts and images come out of the Run's OCI layouts and no registry is involved.

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

## Install

There are three ways to install the stack. All three apply the same deploy path, in the same order, with the same inputs. The deploy path is the interface; the script is the reference way to apply it. [Deploy Path](deploy-repository.md) describes the format.

### Option 1: The Script

One command does the whole install:

```bash
deploy/nvke-deploy.sh install --sitespec sitespec.yaml --kubeconfig site.kubeconfig \
  --release release.json --site-releases releases/
```

It does what is NVKE's to do and reports or prints what is the site's: the StorageClass, who may reach the controller API, and the CLI on your machine stay yours. It runs five steps in order and stops at the first failure, with diagnostics under `$XDG_STATE_HOME/nvke/diagnostics/`. Every step is safe to repeat: fix the cause and run the same command again. `--dry-run` prints every kubectl, helm and clusterctl argv and changes nothing in the cluster. The [deploy README](https://github.com/dsx-ai-factory/nvke/blob/main/deploy/README.md) lists every flag and variable.

1. **Checks readiness.** The tools and the `helm` pin, the checkout, the site spec, the release description, the run directory, the cluster API, the node count and architecture, a control-plane taint the bootstrap should have removed, exactly one default StorageClass, and egress. A failed check names the fix. Nothing is changed. The StorageClass is yours to bring: which provisioner is right is a property of the hardware, so install the CSI driver for it and annotate its class `storageclass.kubernetes.io/is-default-class=true`; a lab can use rancher local-path-provisioner.
2. **Installs Cluster API.** `clusterctl init` with the core and kubeadm providers at `CAPI_VERSION` from `make/tool-versions.mk`. cert-manager comes with it. A Cluster API of another version is an error: `clusterctl init` does not upgrade. See [Upgrade](../operations/upgrade.md).
3. **Installs the site controller.** Renders `deploy/nico` for the site and against the release into a private scratch directory, applies it item by item, and removes the scratch directory. Applies the site's `MachineImage`, `ClusterRelease` and `NodePoolRelease` resources from `--site-releases`. Waits for CAPNICo and the controller and asks the controller's `/readyz` for its database and Temporal.
4. **Secures.** Patches the Cluster API controllers with the SPIRE exec-auth plugin (`controller/hack/apply-capi-spiffe-exec-auth.sh`). `clusterctl upgrade` drops the patch; rerun the install after it.
5. **Verifies.** Asks the controller for `/api/version` from inside the cluster, then prints what you do next by hand.

After the install, on your own machine and under your own policy:

- **Register the site with the CLI.** `nvkectl config set-site --name <site> --url http://<VIP>:30080 --kubeconfig site.kubeconfig`. The install prints the exact command.
- **Admit your client networks.** The chart admits the controller API from pods in the cluster only. A NetworkPolicy in `nke-controller` selecting `app.kubernetes.io/name: nke-controller` that admits your CIDRs on port 8080 opens it to your machines.
- **Publish the release resources** if you did not pass `--site-releases`: a `MachineImage`, a `ClusterRelease` and a `NodePoolRelease`, as [Your first cluster](../first-cluster.md#7-publish-a-release) shows.

A failed helm release is uninstalled before the rerun:

```bash
helm --kubeconfig site.kubeconfig uninstall <release> -n <namespace>
deploy/nvke-deploy.sh install ...
```

### Option 2: By Hand

The same steps, run by you. Take the script apart as far as you like: `deploy/nvke-deploy.sh install --dry-run` prints the exact commands for your site, in order, and nothing below is hidden in it.

1. **Prepare the cluster.** Make sure exactly one StorageClass is default. The bootstrap already removed the control-plane `NoSchedule` taint.
2. **Install Cluster API.** `clusterctl init --core cluster-api:<version> --bootstrap kubeadm:<version> --control-plane kubeadm:<version>`, with the version `make/tool-versions.mk` pins.
3. **Render the deploy path.** `nvkectl render deploy-path --deploy-path deploy/nico --sitespec sitespec.yaml --release release.json --out <dir>` writes a copy resolved for the site and the release into a directory only you can read. It holds the site's credentials; delete it when the install is done.
4. **Apply the items** in the order `nvke-deploy.yaml` lists them. A manifest item is `kubectl apply --server-side -f` on each of its files, or on `kubectl kustomize` of the directory when the render wrote a `kustomization.yaml`. A helm item is `helm upgrade --install <release> <chart> -n <namespace> --wait` with the item's values files in order; the item's `release.yaml` names the chart, release, values and `requires`. Wait for each `requires` entry with `kubectl wait` before its chart. Then apply the site's release resources with `kubectl apply --server-side -f releases/`.
5. **Secure.** Run `controller/hack/apply-capi-spiffe-exec-auth.sh --kubeconfig site.kubeconfig`.
6. **Register and admit.** `nvkectl config set-site --name <site> --url http://<VIP>:30080 --kubeconfig site.kubeconfig`, and a NetworkPolicy in `nke-controller` that admits your client networks to the controller API.
7. **Verify.** `curl http://<VIP>:30080/api/version` from an admitted machine, and `nvkectl get providers`.

### Option 3: GitOps

Every item in `deploy/nico/nvke-deploy.yaml` is a Helm chart plus values files, or plain YAML, so a GitOps tool can deliver the same items without changing the deploy path.

- **Source.** The clone is a valid git source. The controller and NKTP charts are in the tree, at `controller/deploy/charts/controller` and `nktp/deploy/charts/nktp`. Postgres, SPIRE CRDs, Temporal and SPIRE come from the references in the release. An OCI registry is a valid Helm source, so the same references serve Argo CD.
- **Order.** The item list order maps to sync waves, one wave per item.
- **The Certificate.** The `spire` item declares a `requires:` on the `oidc-provider-upstream-ca` Certificate. In GitOps that is a wave boundary or a health check on the Certificate, not a `kubectl wait`.
- **Secrets.** The `nvke-base-config` item renders two Secrets: `nico-credentials` and `dgxc-tenant-credentials`. Under GitOps those come from the operator's secret store. The namespaces and the Certificate chain in the same item are plain YAML and stay in git.
- **Per-site values.** The `site-values.yaml` files the render writes belong in your own repository or your secret store.
- **Imperative steps.** `clusterctl init` and `controller/hack/apply-capi-spiffe-exec-auth.sh` stay imperative, outside GitOps. The other steps do not change.

[Deploy Path](deploy-repository.md#applying-it-another-way) lists what an executor has to do.

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

## Next Steps

- [Your first cluster](../first-cluster.md#7-create-a-tenant-cluster) — create a tenant cluster on the registered site
- [Infrastructure Providers](infrastructure-providers.md) — prove the NICo credentials before committing hardware
