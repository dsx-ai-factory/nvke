# deploy

Installs the NVKE management plane onto a site cluster that `nvkectl bootstrap site` left behind: Postgres, Temporal, SPIRE, CAPNICo, NKTP and the NVKE controller. One scenario: NVKE with CAPNICo on a NICo site.

Two scripts: `check-site.sh` reports what the site still needs, `nvke-deploy.sh` installs the deploy path. Read them before you run them. The guide [Install management-plane services](../docs/operate/install-management-plane-services.md) explains each step.

## Inputs

| Input | From | Says |
|---|---|---|
| The site spec | `nvkectl generate sitespec`, filled in | What is specific to this site: the nodes and VIP, the SPIRE trust domain and CA, and under `controllerStack` the architecture, the NICo API and token, the tenant login, the database password file, optional image overrides, mirror and nktp values. [docs/sitespec.md](../cli/docs/sitespec.md) |
| The release description | `nvkr release export --reference <repo@digest>` | Which published artifacts to use, each by OCI reference and digest: `nvkr.dev/release-description/v1` JSON. It describes the release and verifies nothing; run `nvkr release verify` on the same reference first. Optional; without it the charts come from this checkout and the public repositories, and the site spec names the images |
| The deploy path, [`nico/`](nico/) | This checkout | What to install and in which order. `nvke-deploy.yaml` lists the items; each item directory holds its chart pin (`release.yaml`), values and overlay, or its manifests. [deploy-repository.md](../docs/site-bootstrap/deploy-repository.md) |

The credentials the install reads, the NICo token, the tenant password and the database password, are file paths in the site spec, so the spec holds none of them; `registry.apiKey` is the bootstrap's and comes from the environment, as [Bootstrap the management cluster](../docs/operate/bootstrap-management-cluster.md#write-the-site-spec) says. `nvkectl` reads the deploy path and renders it for the site and against the release into a private scratch directory under `$XDG_STATE_HOME/nvke/renders` (`~/.local/state/nvke/renders`); the script removes it when it exits.

## Run

The install is a list of commands; [Install management-plane services](../docs/operate/install-management-plane-services.md) gives it in order with what each one does. The two scripts here are steps 1 and 3 of that list.

```bash
deploy/check-site.sh --sitespec sitespec.yaml --kubeconfig site.kubeconfig [--release release.json [--nvkr-run-dir DIR]]
clusterctl init --kubeconfig site.kubeconfig --core cluster-api:<v> --bootstrap kubeadm:<v> --control-plane kubeadm:<v> --wait-providers
deploy/nvke-deploy.sh install --deploy-path deploy/nico --sitespec sitespec.yaml --kubeconfig site.kubeconfig [--release release.json [--nvkr-run-dir DIR]]
controller/hack/apply-capi-spiffe-exec-auth.sh --kubeconfig site.kubeconfig
kubectl --kubeconfig site.kubeconfig apply -f nvke-api-clients.yaml   # the NetworkPolicy admitting your client networks; the guide shows it
nvkectl config set-site --name <site> --url http://<VIP>:30080 --kubeconfig site.kubeconfig
nvkectl create machineimage -f machineimage.yaml && nvkectl create clusterrelease -f clusterrelease.yaml && nvkectl create nodepoolrelease -f nodepoolrelease.yaml
```

## check-site.sh

One line per finding, PASS, WARN, SKIP or FAIL with the fix, and a non-zero exit on any FAIL. It changes nothing. When everything passes it prints the commands that remain, `clusterctl init` when Cluster API is not installed yet and `nvke-deploy.sh install`, with the given paths.

| Finding | What it checks |
|---|---|
| tools | `kubectl`, `helm`, `clusterctl`, `curl`, `python3` on `PATH`; `helm` at the pin in `make/tool-versions.mk`; `nvkectl` (`NVKECTL`, else `PATH`) |
| checkout | `deploy/nico/nvke-deploy.yaml` and the controller and NKTP charts, which the executor installs from the tree |
| Cluster API | `CAPI_VERSION` from `make/tool-versions.mk`; `clusterctl` at that version; not installed yet, installed at that version with its kubeadm providers, or a FAIL: another version, a digest-only image, or a core without the kubeadm providers |
| site spec | `nvkectl validate sitespec` passes; the architecture is read for the node check |
| release | `--release` parses and names its reference and declared qualification; `--nvkr-run-dir` exists |
| cluster | the API answers with `--kubeconfig`; an odd node count; the node architecture matches `controllerStack.arch`; no control-plane `NoSchedule` taint; exactly one default StorageClass |
| egress | a fixed list of endpoints answers within 10s: `ghcr.io`, `github.com`, the NKTP chart indexes, `quay.io`, `registry.k8s.io` (WARN) |
| disk | at least 10 GB free under the state directory (WARN) |

`--diagnostics` with `--kubeconfig` instead writes nodes, pods, events, helm releases and the controller logs under `NVKE_STATE_DIR/diagnostics` and exits 0; run it after a failed step.

## nvke-deploy.sh

```bash
deploy/nvke-deploy.sh install --deploy-path DIR [--sitespec FILE] [--release FILE] [--nvkr-run-dir DIR] \
  [--config bootstrap-config.json] [--values-for ITEM=FILE]... [--kubeconfig PATH] [--timeout 15m] [--dry-run] [-- helm args]
```

The executor: it walks `nvke-deploy.yaml` and applies the items in list order. With `--sitespec` or `--release` it first renders the deploy path for the site and against the release into a private scratch directory under `NVKE_STATE_DIR`, installs from there and removes it when it exits, so no rendered credential is left behind. `--dry-run` prints every kubectl and helm argv instead. The e2e lanes install `controller/deploy/capd-e2e` and `capnico-e2e` with it, and `controller/hack/ci/kind-stack-from-release.sh` brings `deploy/nico` up on a kind node from a release through steps 1 to 4 of that list.

| Type | What happens |
|---|---|
| `manifest` | every `.yaml` in the item directory, `kubectl apply --server-side`, in name order; a rendered `kustomization.yaml` is applied through `kubectl kustomize` |
| `template` | rendered for the site by `nvkectl render`; with `--config`, rendered by `deploy/lib/render-template.py` against that BootstrapConfig JSON into a temporary directory, then applied like a manifest item |
| `helm` | one chart, one release, `helm upgrade --install --wait`; `release.yaml` names the chart, version, source, release name, namespace, values files, `setFile` entries and `requires` preconditions |

A chart comes from `chart.path` (a checkout directory, `helm dependency build` first, or the archive a render put under `charts/`), else `chart.repository` at `chart.version`. Values apply in order: the item's own files, then the files a render adds (`site-values.yaml`, and `release-images.yaml` when the release carries the item's images), then every `--values-for`. `images.txt` in a rendered tree names the OCI layouts to import into the node's containerd before anything is applied; `CTR` names the import command (`sudo ctr`, or `docker exec -i <node> ctr` for a kind node).

## Environment

| Variable | Default | Purpose |
|---|---|---|
| `NVKECTL` | `nvkectl` on `PATH` | The binary that reads the deploy path and renders. `make -C cli build` writes `cli/bin/nvkectl` |
| `NVKE_STATE_DIR` | `$XDG_STATE_HOME/nvke` | Render scratch and diagnostics. Never a shared temp directory |
| `CTR` | `ctr` | The containerd import command for `images.txt` |
| `PHASE_TIMEOUT`, `--timeout` | `15m` | `helm --wait` timeout per release |
| `REQUIRE_TIMEOUT` | `180s` | `kubectl wait` and rollout timeout |
| `CAPI_VERSION` | `make/tool-versions.mk` | The Cluster API version `check-site.sh` expects and prints the `clusterctl init` for |
| `CONTROLLER_NAMESPACE` | `nke-controller` | Where `check-site.sh --diagnostics` reads the controller logs |

## Test

```bash
make -C deploy test   # builds nvkectl, renders deploy/nico for a site and against a fake Run, runs check-site.sh against stub tools and the executor in --dry-run
```
