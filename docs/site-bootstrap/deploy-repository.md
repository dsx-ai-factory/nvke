# Deploy Path

A deploy path is a directory that lists the NVKE services in install order. It holds one `nvke-deploy.yaml` and one directory per item. An item is a Helm chart with its values, a set of plain manifests, or templates that render the site's secrets.

NVKE ships one deploy path, [`deploy/nico`](https://github.com/dsx-ai-factory/nvke/tree/main/deploy/nico). `deploy/nvke-deploy.sh install` applies it, rendered for the site spec and the NVKR release, as one step of the install. See [Install management-plane services](../operate/install-management-plane-services.md) for the list and its inputs.

The deploy path is the interface. The script is the reference way to apply it. An operator can apply the same items with their own tooling, or through GitOps. `deploy/nvke-deploy.sh install --dry-run` prints every kubectl and helm call the script would make, in order.

## Layout

```text
deploy/nico/
├── nvke-deploy.yaml              # the items, in install order
├── nvke-base-config/             # template: namespaces, the SPIRE CA chain, two Secrets
│   ├── 00-namespaces.yaml
│   ├── 20-nico-credentials.yaml.tmpl
│   ├── 35-dgxc-tenant-credentials.yaml.tmpl
│   ├── 40-oidc-upstream-ca.yaml
│   └── 45-nktp-cluster.yaml.tmpl
├── spire-server-nodeport/        # manifest: a Service
│   └── spire-server-nodeport.yaml
├── capnico/                      # manifest: the CAPNICo provider, bound to its image
│   ├── capnico.yaml
│   └── release.yaml
├── nktp/                         # helm: a chart from this checkout
│   ├── release.yaml
│   └── values.yaml
├── temporal/                     # helm: a chart from a public repository
│   ├── release.yaml
│   ├── values.yaml
│   └── overlay.yaml
└── ...
```

Only the directories that `nvke-deploy.yaml` names are read. Other files at the root are ignored.

## The `nvke-deploy.yaml` File

```yaml
items:
  - {name: nvke-base-config, type: template}
  - {name: spire-tpm-pubhashes, type: manifest}
  - {name: spire-server-nodeport, type: manifest}
  - {name: spire-oidc-provider-nodeport, type: manifest}
  - {name: capnico, type: manifest}
  - {name: nktp, type: helm}
  - {name: postgresql, type: helm}
  - {name: spire-crds, type: helm}
  - {name: temporal, type: helm}
  - {name: spire, type: helm}
  - {name: controller, type: helm}
```

| Field | Required | Description |
|-------|----------|-------------|
| `name` | Yes | The item's directory under the deploy path. For a `helm` item, also the default NVKR artifact name of its chart. Letters, digits, `-`, `_` and inner dots. |
| `type` | Yes | `template`, `manifest` or `helm`. |

The list order is the install order. Items are applied one at a time, and a failed item stops the run. Nothing is auto-detected: an item that is not listed is not applied, and `nvkectl` and the executor's own parser both refuse an unknown type before anything is applied.

The script reads the item list and an item's `release.yaml` through `deploy/lib/nvke-deploy-items.py`; `nvkectl render deploy-path` reads them through `cli/internal/deploypath`. Both accept the grammar shown here.

## Item Types

### `template`: the site's secrets

A template item holds `.yaml.tmpl` files, and may hold plain `.yaml` files beside them. Each `.yaml.tmpl` is rendered with Go's [`text/template`](https://pkg.go.dev/text/template), the plain files are copied, and the result is applied as a manifest item. This is how the deploy path creates the Secrets the stack needs without committing a credential.

The template context is what the site spec's `controllerStack` section supplies:

| Expression | Taken from |
|------------|------------|
| `.ClusterName` | `clusterName` |
| `.Nico.Endpoint`, `.Nico.OrgID`, `.Nico.APIName` | `controllerStack.nico.endpoint`, `.orgID`, `.apiName` |
| `.Nico.Token` | The file `controllerStack.nico.tokenFile` names |
| `.Tenant.User` | `controllerStack.tenant.user` |
| `.Tenant.Password` | The file `controllerStack.tenant.passwordFile` names |

A template that names another field is refused, and so is an empty value: it would render an empty credential. A value that holds a quote, a backslash or a control character is refused too, because every value lands inside a quoted YAML scalar.

`deploy/nico/nvke-base-config/20-nico-credentials.yaml.tmpl` renders the Secret CAPNICo reads:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: nico-credentials
  namespace: capnico-system
type: Opaque
stringData:
  endpoint: "{{ .Nico.Endpoint }}"
  orgID: "{{ .Nico.OrgID }}"
  token: "{{ .Nico.Token }}"
  apiName: "{{ .Nico.APIName }}"
```

Files are applied in name order. Use numeric prefixes (`00-`, `20-`, `40-`) so that namespaces exist before anything lands in them.

Without a site spec, `install --config <file>` renders the templates from a JSON document with the same field names. The e2e lanes do this; a site install does not need it.

### `manifest`: plain Kubernetes manifests

A manifest item holds `.yaml` files. Each one is applied with `kubectl apply --server-side`, in name order.

A manifest item may carry a `release.yaml` that binds the image its YAML runs to an NVKR artifact. `deploy/nico/capnico/release.yaml`:

```yaml
image: ghcr.io/dsx-ai-factory/cluster-api-provider-nico/controller:v0.0.45
artifact: capnico-controller
```

`image` is the repository and tag the committed YAML names. `artifact` is the NVKR output that publishes it. A render against a release writes a `kustomization.yaml` that repoints the image to the artifact's repository and digest, and the script applies the kustomization instead of the files. `release.yaml` itself is never applied.

### `helm`: one chart as one release

A helm item installs one chart as one Helm release. Its `release.yaml` names the chart, the release and the values:

```yaml
chart:
  name: temporal            # the chart's name
  version: "0.74.0"         # required for a repository chart; a path chart names none
  repository: https://go.temporal.io/helm-charts   # or path: ../../nktp/deploy/charts/nktp
  artifact: temporal        # NVKR artifact of the chart; default: the item name
  imageArtifact: cwe        # NVKR artifact of the image the chart runs, if any
  profilesArtifact: controller-profiles             # and of its profiles content image
  setFile:                  # <helm key>=<absolute path>, passed as --set-file
    - some.chart.key=/etc/nvke/some-file
release:
  name: temporal
  namespace: temporal
values:
  - values.yaml             # applied in order; later files win
  - overlay.yaml
requires:
  - {kind: certificate, name: oidc-provider-upstream-ca, namespace: spire-server, condition: Ready}
```

| Field | Description |
|-------|-------------|
| `chart.repository` or `chart.path` | Where the chart comes from. One of the two. A repository is an `https://` index or an `oci://` reference. A path is relative to the deploy path, so from `deploy/nico` a chart in this checkout is reachable as `../../controller/deploy/charts/controller`. |
| `chart.version` | Selects the chart in a repository. A path chart names no version: the chart at the path carries its own. |
| `chart.artifact`, `chart.imageArtifact`, `chart.profilesArtifact` | Bind the item to an NVKR release by NVKR's output names: the chart, the image the chart runs, and its profiles content image. |
| `chart.setFile` | Files the operator holds, passed to helm as `--set-file`. The deploy path carries the path, never the content. |
| `release.name`, `release.namespace` | The Helm release. The namespace must already exist; `nvke-base-config` creates the ones `deploy/nico` uses. |
| `values` | Values files in the item directory, applied in order. A render appends `site-values.yaml`, and `release-images.yaml` when the release carries the item's images. `install --values-for <item>=<file>` adds a file after those. |
| `requires` | Objects that must exist and reach a condition before the chart installs. `helm --wait` waits on workloads only, so a Certificate issued by an earlier item is declared here and waited on with `kubectl wait`. |

The install runs `helm upgrade --install --wait` for each item, with the item's namespace and values, and retries a chart download that fails with a server error. With a release, the release decides the version of every chart it carries, and a `release.yaml` that pins another version gets a warning. The pin decides only for a chart the release lacks.

## Rendering for a Site and a Release

The committed deploy path is site-independent and names no digest. `nvkectl render deploy-path` writes a copy resolved for the site spec, the release, or both. The committed files are copied, never edited. The copy has:

- `site-values.yaml` in every item that takes a site value, appended to its `values`: the trust domain, the management VIP, the node architecture's TPM plugin checksum, the database password, the controller and profiles images. [`deploy/nico/README.md`](https://github.com/dsx-ai-factory/nvke/blob/main/deploy/nico/README.md#per-site-values) lists them.
- Each template item rendered, as a manifest item.
- `charts/<item>/` holding the chart archive pulled by digest, for every chart the release carries; the item's `chart.path` points at it. A chart the release lacks stays as committed.
- `release-images.yaml` in every helm item whose image artifacts the release carries; `kustomization.yaml` in every manifest item bound to an image artifact.
- At the root: `images.txt`, the images the result runs; `nvkr-release.json`, a copy of the release; and a `.gitignore` that ignores the tree.

The copy holds the site's credentials. It is written readable by its owner only, and the render refuses an output under a shared temp directory. `nvke-deploy.sh` renders into a scratch directory under `NVKE_STATE_DIR` (default `$XDG_STATE_HOME/nvke`) and removes it when it exits, so no rendered credential is left behind. To read a render, run the command yourself:

```bash
nvkectl render deploy-path --deploy-path deploy/nico --sitespec sitespec.yaml --release release.json \
  --out ~/.local/state/nvke/renders/my-site
```

## Applying It Another Way

Every item is a Helm chart with values, or plain YAML, so another tool can deliver the same items. What the script does beyond `helm` and `kubectl apply` is small and listed here, so another executor can do the same:

1. Apply the items in list order; stop at the first failure.
2. Render the template items first. Under GitOps, the Secrets they produce come from your secret store instead.
3. Wait for each `requires` entry before its chart. In GitOps that is a sync-wave boundary or a health check on the object.
4. With a local NVKR Run (`--nvkr-run-dir`), import the images listed in `images.txt` into the node's containerd before the first item.

A GitOps tool maps these onto sync waves and a secret store; `nvkectl render deploy-path` writes the per-site values and the rendered template items it delivers. The steps around the deploy path, such as `clusterctl init` and the SPIRE exec-auth patch, are in the same guide and stay imperative.

## Adding an Item

1. Create a directory under the deploy path, named for the item.
2. Add its files: a `release.yaml` and values for a helm item, `.yaml` files for a manifest item, `.yaml.tmpl` files for a template item.
3. Add an entry to `nvke-deploy.yaml` at the position the install order needs. An item that creates a monitor goes after `nktp`, which installs the monitoring CRDs; an item that needs a namespace goes after `nvke-base-config`.
4. Run `hack/deploy-lists-check.sh`, which checks that every listed item has a directory.
