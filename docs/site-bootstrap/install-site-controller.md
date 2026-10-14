# Install the Site Controller

`nvkectl bootstrap site` leaves a plain Kubernetes cluster. This page installs the management plane onto it, as a short list of commands: Postgres, Temporal, SPIRE, [CAPNICo](https://github.com/dsx-ai-factory/cluster-api-provider-nico), NKTP and the NVKE controller.

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

`export` verifies nothing, so run it only on a reference `verify` accepted. With the description, the charts the release carries are installed by digest and the images are pinned by digest; a chart the release lacks comes from the checkout or its public repository. Without `--release`, every chart comes from the checkout and the public repositories, and the site spec names the controller and profiles images under `controllerStack.images`. For a local build, add `--nvkr-run-dir .nvkr/runs/<attempt>`: the charts and images the release carries come out of the Run's OCI layouts, and the images are imported into the one containerd that `CTR` reaches; a chart the release lacks still comes from its repository.

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

The install is a list of commands, run in this order. Each one stands on its own; nothing orchestrates them. What is the site's to decide, the StorageClass, who may reach the controller API, the CLI on your machine, is yours to do, and the first command tells you what is still missing.

1. **Check the site.** `deploy/check-site.sh --sitespec sitespec.yaml --kubeconfig site.kubeconfig --release release.json` prints one line per finding, with the fix: the tools and the `helm` pin, the checkout, the site spec, the release description and its declared qualification, the cluster API, an odd node count, the node architecture against `controllerStack.arch`, the control-plane taint the bootstrap removes, exactly one default StorageClass, the Cluster API version, egress and disk. It changes nothing and exits non-zero on a FAIL. When everything passes it prints the commands that remain, with your paths filled in. The StorageClass is yours to bring: which provisioner is right is a property of the hardware, so install the CSI driver for it and annotate its class `storageclass.kubernetes.io/is-default-class=true`; a lab can use rancher local-path-provisioner.
2. **Install Cluster API.** `clusterctl init --kubeconfig site.kubeconfig --core cluster-api:<version> --bootstrap kubeadm:<version> --control-plane kubeadm:<version> --wait-providers`, with the version `make/tool-versions.mk` pins; the check prints the exact line. cert-manager comes with it. `clusterctl init` does not upgrade, so a Cluster API of another version is a finding in step 1, not something to install over. See [Upgrade](../operations/upgrade.md).
3. **Install the stack.** `deploy/nvke-deploy.sh install --deploy-path deploy/nico --sitespec sitespec.yaml --kubeconfig site.kubeconfig --release release.json` renders `deploy/nico` for the site and against the release into a private scratch directory, applies it item by item with `--wait`, and removes the scratch directory. `--dry-run` prints every kubectl and helm call instead. A failed helm release is uninstalled before the rerun: `helm --kubeconfig site.kubeconfig uninstall <release> -n <namespace>`.
4. **Patch the Cluster API controllers for SPIRE.** `controller/hack/apply-capi-spiffe-exec-auth.sh --kubeconfig site.kubeconfig` gives them the SPIFFE exec-auth plugin, so Cluster API reaches tenant clusters with a SPIFFE JWT rather than a stored admin kubeconfig. After step 3, because the script checks the rollout only once the SPIRE CSI driver, which the `spire` item installs, is registered. `clusterctl upgrade` drops the patch; run the script again after it.
5. **Register the site and admit your networks.** On the machine the CLI lives on: `nvkectl config set-site --name <site> --url http://<VIP>:30080 --kubeconfig site.kubeconfig`. Port 30080 is the plain-HTTP NodePort the chart exposes and the CLI sends a bearer token to it, so use it from a trusted network only. The chart admits the controller API from pods in the cluster only; a NetworkPolicy in `nke-controller` selecting `app.kubernetes.io/name: nke-controller` that admits your client CIDRs on port 8080 opens it to your machines.
6. **Publish the release resources.** `nvkectl create machineimage -f machineimage.yaml`, then `nvkectl create clusterrelease -f clusterrelease.yaml` and `nvkectl create nodepoolrelease -f nodepoolrelease.yaml`, in that order. They write the resources to the management cluster through the kubeconfig `set-site` recorded, so they need the registration but not the network policy. The files are `nvkectl create` inputs, not manifests; [Your first cluster](../first-cluster.md#7-publish-a-release) shows the three and the examples to copy. The chart installed their CRDs in step 3.
7. **Verify.** `nvkectl get providers` and `nvkectl get machineimages`, and `curl http://<VIP>:30080/api/version` from an admitted machine.

After a failure in step 3, `deploy/check-site.sh --kubeconfig site.kubeconfig --diagnostics` writes nodes, pods, events, helm releases and the controller logs under `$XDG_STATE_HOME/nvke/diagnostics/`.

The kind lane, `controller/hack/ci/kind-stack-from-release.sh`, runs steps 1, 2, 3 and 4 in this order on a kind node from a three-artifact release, so the list above is executed in CI.

### Taking the executor apart

Step 3 is two things you can run yourself. `nvkectl render deploy-path --deploy-path deploy/nico --sitespec sitespec.yaml --release release.json --out <dir>` writes the copy resolved for the site and the release into a directory only you can read; it holds the site's credentials, so delete it when you are done. Then apply the items in the order `nvke-deploy.yaml` lists them: a manifest item is `kubectl apply --server-side -f` on each of its files except `release.yaml`, or on `kubectl kustomize` of the directory when the render wrote a `kustomization.yaml`; a helm item is `helm upgrade --install <release> <chart> -n <namespace> --wait` with the item's values files in order, and its `release.yaml` names the chart, release, values and `requires`. A path chart with a `Chart.lock`, which the NKTP chart has, needs `helm dependency build` first. Each `chart.setFile` entry an item declares is passed as `--set-file`; an item that declares none has none. Wait for each `requires` entry with `kubectl wait` before its chart. `deploy/nvke-deploy.sh install --dry-run` prints exactly this for your site.

### GitOps

Every item in `deploy/nico/nvke-deploy.yaml` is a Helm chart plus values files, or plain YAML, so a GitOps tool can deliver the same items without changing the deploy path.

- **Source.** The clone is a valid git source. The controller and NKTP charts are in the tree, at `controller/deploy/charts/controller` and `nktp/deploy/charts/nktp`. Postgres, SPIRE CRDs, Temporal and SPIRE come from the references in the release. An OCI registry is a valid Helm source, so the same references serve Argo CD.
- **Order.** The item list order maps to sync waves, one wave per item.
- **The Certificate.** The `spire` item declares a `requires:` on the `oidc-provider-upstream-ca` Certificate. In GitOps that is a wave boundary or a health check on the Certificate, not a `kubectl wait`.
- **Rendered objects.** The `nvke-base-config` item renders two Secrets, `nico-credentials` and `dgxc-tenant-credentials`, and the `nktp-cluster` ConfigMap, which the NKTP collectors read through a required `configMapKeyRef` and do not start without. Under GitOps the Secrets come from the operator's secret store and the ConfigMap is committed with the site's name. The namespaces and the Certificate chain in the same item are plain YAML and stay in git.
- **Charts.** A path chart with a `Chart.lock` needs its dependencies built, and a `chart.setFile` entry becomes a values override, the same as by hand.
- **Per-site values.** `nvkectl render deploy-path --deploy-path deploy/nico --sitespec … --release … --out <dir>` writes the `site-values.yaml` files and the rendered template items; commit what it writes, minus the Secrets, to your repository or your secret store.
- **Outside GitOps.** Steps 1, 2, 4, 5 and 6 stay as they are: a check, `clusterctl init`, the exec-auth patch, and your own machine for the registration and the releases.

[Deploy Path](deploy-repository.md#applying-it-another-way) lists what an executor has to do.

## Remove

Uninstall in reverse item order, then remove Cluster API:

```bash
export KUBECONFIG=site.kubeconfig
helm uninstall nke-controller -n nke-controller
helm uninstall spire -n spire-server
helm uninstall temporal -n temporal
helm uninstall spire-crds -n spire-server
helm uninstall postgres -n postgres
helm uninstall nktp -n nktp
kubectl delete -f deploy/nico/capnico/capnico.yaml
clusterctl delete --all
kubectl delete namespace nke-controller nke-system nktp postgres temporal spire-server spire-system capnico-system
```

The last line removes what the manifest and template items created and what Helm leaves behind: the `nico-credentials` and `dgxc-tenant-credentials` Secrets, the `nktp-cluster` ConfigMap, the SPIRE Services and TPM hashes, the Certificate chain, and the PersistentVolumeClaims, whose volumes go with them under the default reclaim policy. `cert-manager` belongs to Cluster API and stays until `clusterctl delete --all --include-crd` or your own cleanup. The `capi-spiffe-exec-auth` patch goes with the Cluster API controllers.

Deleting the namespaces deletes the PersistentVolumeClaims with them. Take a copy of the Postgres data first if you still need it.

## Upgrade

Verify and export the new release, then run the check and the executor again with the new description:

```bash
deploy/check-site.sh --sitespec sitespec.yaml --kubeconfig site.kubeconfig --release new-release.json
deploy/nvke-deploy.sh install --deploy-path deploy/nico --sitespec sitespec.yaml --kubeconfig site.kubeconfig --release new-release.json
```

The check reports Cluster API at its version; the stack is upgraded in place. [Tenant Cluster Kubernetes Upgrades](../operations/upgrade.md) covers upgrades of tenant clusters.

## Next Steps

- [Your first cluster](../first-cluster.md#8-create-a-tenant-cluster) — create a tenant cluster on the registered site
- [Infrastructure Providers](infrastructure-providers.md) — prove the NICo credentials before committing hardware
