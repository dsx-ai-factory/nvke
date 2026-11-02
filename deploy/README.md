# deploy

Installs the NVKE management plane onto a site cluster that `nvkectl bootstrap site` left behind: Postgres, Temporal, SPIRE, CAPNICo, NKTP and the NVKE controller. One scenario: NVKE with CAPNICo on a NICo site.

Two scripts: `check-site.sh` reports what the site still needs, `nvke-deploy.sh` installs the deploy path. `nvke-deploy-local.sh` wraps the second for the local site of [Run NVKE end to end on one machine](../docs/get-started/local-site.md): the lab StorageClass before, the inputs from the environment, the SPIRE exec-auth patch after. Read them before you run them.

## Inputs

| Input | From | Says |
|---|---|---|
| The site spec | `nvkectl generate sitespec`, the file the bootstrap ran with | What the bootstrap built and the stack must agree with: the site name, the control-plane VIP and the SPIRE trust domain. The install reads it and changes nothing in it. [docs/sitespec.md](../cli/docs/sitespec.md) |
| The stack config, `stack-config.json` | [`examples/stack-config.json`](examples/stack-config.json), copied and filled in | What only the stack needs: the NICo API, and optionally image overrides and a mirror. See [The stack config](#the-stack-config) |
| The release description | `nvkr release export --reference <repo@digest>` | Which published artifacts to use, each by OCI reference and digest: `nvkr.dev/release-description/v1` JSON. It describes the release and verifies nothing; run `nvkr release verify` on the same reference first. Optional; without it the charts come from this checkout and the public repositories, and the stack config names the images |
| The deploy path, [`nico/`](nico/) | This checkout | What to install and in which order. `nvke-deploy.yaml` lists the items; each item directory holds its chart pin (`release.yaml`), values and overlay, or its manifests. [deploy-repository.md](../docs/site-bootstrap/deploy-repository.md) |

The install reads the NICo token, the node login and the database password from `NVKECTL_NICO_TOKEN`, `NVKECTL_NODE_USER`, `NVKECTL_NODE_PASSWORD` and `NVKECTL_DATABASE_PASSWORD`, and the SPIRE CA from the files `NVKECTL_SPIRE_CA_CERT_FILE` and `NVKECTL_SPIRE_CA_KEY_FILE` name, which a site spec with `spire.upstreamCA: true` requires. The render writes them into Secrets (`nvke-database`, `spire-upstream-ca`, the template items') that the charts read by name; no values file carries a credential. The nodes' CPU architecture is in no file: both scripts read it from the cluster's `kubernetes.io/arch` node labels, and the executor passes it to the render, which selects the release artifacts and the SPIRE TPM attestor checksum for it. `nvkectl` reads the deploy path and renders it for the site and against the release into a private scratch directory under `$XDG_STATE_HOME/nvke/renders` (`~/.local/state/nvke/renders`); the script removes it when it exits.

## Run

[Run NVKE end to end on one machine](../docs/get-started/local-site.md) gives
the install in order with what each command does. `nvke-deploy-local.sh` is
the entry point; it calls `nvke-deploy.sh` with the inputs from the
environment.

```bash
clusterctl init --kubeconfig local-site.kubeconfig --core cluster-api:<v> --bootstrap kubeadm:<v> --control-plane kubeadm:<v> --wait-providers
"$NVKE_REPO"/deploy/nvke-deploy-local.sh install
kubectl --kubeconfig local-site.kubeconfig apply -f nvke-api-clients.yaml   # the NetworkPolicy admitting your client networks
nvkectl config set-site --name <site> --url http://<ADDRESS>:30080 --kubeconfig local-site.kubeconfig
nvkectl create machineimage -f machineimage.yaml && nvkectl create clusterrelease -f clusterrelease.yaml && nvkectl create nodepoolrelease -f nodepoolrelease.yaml
```

The two scripts below are the reference documentation for `check-site.sh` and
`nvke-deploy.sh`, which `nvke-deploy-local.sh` wraps.

## The stack config

One JSON object, the keys of [`examples/stack-config.json`](examples/stack-config.json) and no others: an unknown, duplicate or differently cased key is refused, as is a value that is still a `REPLACE_*` placeholder. Copy the example, fill it in, and keep it with the site spec; it holds no secret. `nvkectl inspect stack-config stack-config.json` (internal; both scripts run it) reports every problem at once as `<key>: <problem>`, and checks the `NVKECTL_*` credentials with it.

| Key | Required | Value |
|---|---|---|
| `nico.endpoint` | yes | The NICo API base URL CAPNICo calls, `http` or `https`, with no user or password |
| `nico.orgID` | yes | The NICo organization ID |
| `nico.apiName` | yes | The API name in the NICo API path, for example `forge` |
| `images.controller` | no | Overrides the controller image, as `repository:tag` or `repository@sha256:<digest>`; defaults to the release's `cwe` artifact, or without a release to the overlay's pinned image |
| `images.profiles` | no | Overrides the profile content image; defaults to the release's `controller-profiles` artifact. Required without a release: `deploy/nico` enables the image without naming it |
| `mirror` | no | A registry the external images are pulled from instead of their upstream registries, as `host[:port][/prefix]`, no scheme |

## check-site.sh

One line per finding, PASS, WARN, SKIP or FAIL with the fix, and a non-zero exit on any FAIL. It changes nothing. It starts with an `INFO  inputs:` line naming the site spec and stack config it pairs, by absolute path, with the site name and the NICo endpoint and org, so a wrong file is seen before anything is checked. When everything passes it prints the commands that remain, `clusterctl init` when Cluster API is not installed yet and `nvke-deploy.sh install`, with the given paths.

| Finding | What it checks |
|---|---|
| tools | `kubectl`, `helm`, `clusterctl`, `curl`, `python3` on `PATH`; `helm` at the pin in `make/tool-versions.mk`; `nvkectl` (`NVKECTL`, else `PATH`) |
| checkout | `deploy/nico/nvke-deploy.yaml` and the controller and NKTP charts, which the executor installs from the tree |
| Cluster API | `CAPI_VERSION` from `make/tool-versions.mk`; `clusterctl` at that version; not installed yet, installed at that version with its kubeadm providers, or a FAIL: another version, a digest-only image, or a core without the kubeadm providers |
| site spec | `nvkectl validate sitespec` passes |
| stack config | `nvkectl inspect stack-config` accepts the file and finds the `NVKECTL_*` credentials: strict JSON, no placeholder, every required value set and well-formed, every variable set. A FAIL carries the first `<key>: <problem>` |
| release | `--release` parses and names its reference and declared qualification; `--nvkr-run-dir` exists |
| cluster | the API answers with `--kubeconfig`; an odd node count; every node carries `kubernetes.io/arch` and all report one architecture; no control-plane `NoSchedule` taint; exactly one default StorageClass |
| egress | a fixed list of endpoints answers within 10s: `ghcr.io`, `github.com`, the NKTP chart indexes, `quay.io`, `registry.k8s.io` (WARN) |
| disk | at least 10 GB free under the state directory (WARN) |

`--diagnostics` with `--kubeconfig` instead writes nodes, pods, events, helm releases and the controller logs under `NVKE_STATE_DIR/diagnostics` and exits 0; run it after a failed step.

## nvke-deploy.sh

```bash
deploy/nvke-deploy.sh install --deploy-path DIR [--sitespec FILE --stack-config FILE [--arch linux/ARCH]] [--release FILE] [--nvkr-run-dir DIR] \
  [--config bootstrap-config.json] [--values-for ITEM=FILE]... [--kubeconfig PATH] [--timeout 15m] [--dry-run] [-- helm args]
```

The executor: it walks `nvke-deploy.yaml` and applies the items in list order. With `--sitespec` and `--stack-config`, or `--release`, it first renders the deploy path for the site and against the release into a private scratch directory under `NVKE_STATE_DIR`, installs from there and removes it when it exits, so no rendered credential is left behind. `--sitespec` and `--stack-config` go together, and `--release` needs both. Before the render it logs the same `inputs:` line as the check, reads the nodes' architecture from the cluster and passes it to the render as `--arch`; `--arch linux/amd64` or `--arch linux/arm64` on the executor stands in for the read. No nodes, a node without the `kubernetes.io/arch` label, two architectures or an unreachable API stop the install; a `--dry-run` with an unreachable API renders for this machine's architecture and says so. `--dry-run` prints every kubectl and helm argv instead. The e2e lanes install `controller/deploy/capd-e2e` and `capnico-e2e` with it, and `controller/hack/ci/kind-stack-from-release.sh` brings `deploy/nico` up on a kind node from a release through steps 1 to 4 of that list.

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
| `CONTROLLER_NAMESPACE` | `nvke-controller` | Where `check-site.sh --diagnostics` reads the controller logs |

## Test

```bash
make -C deploy test   # builds nvkectl, renders deploy/nico for a site and against a fake Run, runs check-site.sh against stub tools and the executor in --dry-run
```
