# deploy

Installs the NVKE management plane onto a site cluster that `nvkectl bootstrap site` left behind: Postgres, Temporal, SPIRE, CAPNICo, NKTP and the NVKE controller. One scenario: NVKE with CAPNICo on a NICo site.

One script, `nvke-deploy.sh`, two commands. Read it before you run it. The guide [docs/site-bootstrap/install-site-controller.md](../docs/site-bootstrap/install-site-controller.md) explains each step.

## Inputs

| Input | From | Says |
|---|---|---|
| The site spec | `nvkectl generate sitespec`, filled in | What is specific to this site: the nodes and VIP, the SPIRE trust domain and CA, and under `controllerStack` the architecture, the NICo API and token, the tenant login, the database password file, optional image overrides, mirror and nktp values. [docs/sitespec.md](../cli/docs/sitespec.md) |
| The release description | `nvkr release export --reference <repo@digest>` | Which published artifacts to use, each by OCI reference and digest: `nvkr.dev/release-description/v1` JSON. It describes the release and verifies nothing; run `nvkr release verify` on the same reference first. Optional; without it the charts come from this checkout and the public repositories, and the site spec names the images |
| The deploy path, [`nico/`](nico/) | This checkout | What to install and in which order. `nvke-deploy.yaml` lists the items; each item directory holds its chart pin (`release.yaml`), values and overlay, or its manifests. [deploy-repository.md](../docs/site-bootstrap/deploy-repository.md) |

The site spec names credential files; it holds no credential itself. `nvkectl` reads the deploy path and renders it for the site and against the release into a private scratch directory under `$XDG_STATE_HOME/nvke/renders` (`~/.local/state/nvke/renders`); the script removes it when it exits.

## Run

```bash
deploy/nvke-deploy.sh install --sitespec sitespec.yaml --kubeconfig site.kubeconfig \
  [--release release.json [--nvkr-run-dir .nvkr/runs/<attempt>]] \
  [--site-releases releases/] [--dry-run]
```

One run, five steps, in this order. The install does what is NVKE's and reports or prints what is the site's: the StorageClass, who may reach the controller API and the CLI on your machine stay yours. The first failure stops the run and writes diagnostics under `$XDG_STATE_HOME/nvke/diagnostics/`. Every step is safe to repeat: fix the cause and run the same command again. `--dry-run` prints every kubectl, helm and clusterctl argv and changes nothing in the cluster.

| Step | Does |
|---|---|
| check readiness | tools on `PATH` and `helm` at the pin; the checkout; `nvkectl validate sitespec`; the release description parses; the run directory; the cluster API, an odd node count, the node architecture against `controllerStack.arch`, no control-plane taint, exactly one default StorageClass; egress to the chart repositories and registries. Changes nothing |
| install Cluster API | `clusterctl init` with the core and kubeadm providers at `CAPI_VERSION` from `make/tool-versions.mk`; cert-manager comes with it. Another installed version is an error |
| install the site controller | `apply` of `deploy/nico` rendered for the site and against the release; the site's release resources from `--site-releases`; waits for CAPNICo and the controller and asks the controller's `/readyz` |
| secure | `controller/hack/apply-capi-spiffe-exec-auth.sh` |
| verify | `GET /api/version` from inside the cluster, then the next steps for the operator: `nvkectl config set-site`, a NetworkPolicy for their client networks, the release resources if none were passed |

## apply

```bash
deploy/nvke-deploy.sh apply --deploy-path DIR [--sitespec FILE] [--release FILE] [--nvkr-run-dir DIR] \
  [--config bootstrap-config.json] [--values-for ITEM=FILE]... [--kubeconfig PATH] [--timeout 15m] [--dry-run] [-- helm args]
```

`apply` is what the install runs for the stack, on its own: it walks `nvke-deploy.yaml` and applies the items in list order. The e2e lanes install `controller/deploy/capd-e2e` and `capnico-e2e` with it, and `controller/hack/ci/kind-stack-from-release.sh` brings `deploy/nico` up on a kind node from a release.

| Type | What happens |
|---|---|
| `manifest` | every `.yaml` in the item directory, `kubectl apply --server-side`, in name order; a rendered `kustomization.yaml` is applied through `kubectl kustomize` |
| `template` | rendered for the site by `nvkectl render`; with `--config`, rendered by `deploy/lib/render-template.py` against that BootstrapConfig JSON into a temporary directory, then applied like a manifest item |
| `helm` | one chart, one release, `helm upgrade --install --wait`; `release.yaml` names the chart, version, source, release name, namespace, values files, `setFile` entries and `requires` preconditions |

A chart comes from `chart.path` (a checkout directory, `helm dependency build` first, or the archive a render put under `charts/`), else `chart.repository` at `chart.version`. Values apply in order: the item's own files, then `site-values.yaml` when rendered, then every `--values-for`. `images.txt` in a rendered tree names the OCI layouts to import into the node's containerd before anything is applied; `CTR` names the import command (`sudo ctr`, or `docker exec -i <node> ctr` for a kind node).

## Environment

| Variable | Default | Purpose |
|---|---|---|
| `NVKECTL` | `nvkectl` on `PATH` | The binary that reads the deploy path and renders. `make -C cli build` writes `cli/bin/nvkectl` |
| `NVKE_STATE_DIR` | `$XDG_STATE_HOME/nvke` | Render scratch and diagnostics. Never a shared temp directory |
| `CTR` | `ctr` | The containerd import command for `images.txt` |
| `PHASE_TIMEOUT`, `--timeout` | `15m` | `helm --wait` timeout per release |
| `REQUIRE_TIMEOUT` | `180s` | `kubectl wait` and rollout timeout |
| `CAPI_VERSION` | `make/tool-versions.mk` | The Cluster API version the install installs |
| `CONTROLLER_NAMESPACE` | `nke-controller` | Where the install reads the controller |

## Test

```bash
make -C deploy test   # builds nvkectl, renders deploy/nico for a site and against a fake Run, runs install and apply in --dry-run
```
