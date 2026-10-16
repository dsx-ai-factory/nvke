# The stack artifacts file

`stack-artifacts.json` names every image and Helm chart the controller stack install reads, by OCI reference and digest. `deploy/check-site.sh` and `deploy/nvke-deploy.sh install` take it as `--stack-artifacts`, and `nvkectl bootstrap site --stack-artifacts` reads its CAPNICo image. Three things write it: an operator, from [`deploy/examples/stack-artifacts.json`](../../deploy/examples/stack-artifacts.json); `make local-stack-artifacts`, from the checkout; and `hack/nvkr-run-to-stack-artifacts.py`, from an NVKR release export.

## Shape

```json
{
  "schemaVersion": "nvke.nvidia.com/stack-artifacts/v1",
  "source": {
    "reference": "ghcr.io/example/recipes/releases@sha256:<64 hex>",
    "declaredQualification": "pipeline-attested"
  },
  "artifacts": [
    {"name": "cwe", "kind": "oci", "reference": "registry.example.com/nvke/nke-controller@sha256:<64 hex>"},
    {"name": "controller-chart", "kind": "helm-oci", "chartVersion": "0.4.1", "reference": "registry.example.com/nvke/charts/nke-controller@sha256:<64 hex>"},
    {"name": "capnico-controller", "kind": "oci", "os": "linux", "arch": "arm64", "reference": "nvke.local/capnico-controller@sha256:<64 hex>", "layout": "images/capnico-controller"}
  ]
}
```

| Field | Required | Value |
|---|---|---|
| `schemaVersion` | yes | `nvke.nvidia.com/stack-artifacts/v1`. An NVKR release description, `nvkr.dev/release-description/v1`, is refused with a pointer at the converter. |
| `source` | no | Where the artifacts came from. `reference` is an OCI reference with a digest; `declaredQualification` is the qualification its publisher declared, a token. Informational: nothing in NVKE verifies it. A block without a `reference` counts as absent. |
| `artifacts` | yes | One entry per artifact, names unique. Every other top-level key is refused. |

Per artifact:

| Field | Required | Value |
|---|---|---|
| `name` | yes | The name a deploy-path item binds: `cwe`, `controller-profiles`, `capnico-controller`, `controller-chart`, `nktp`, or a chart item's name. A name the deploy path does not bind is allowed and noted. |
| `kind` | yes | `oci` for an image, `helm-oci` for a chart. An item binds an artifact only in the role its kind takes; an artifact of another kind under a bound name stops the install. Other kinds are allowed where no item binds the name. |
| `digest` | unless `reference` carries it | `sha256:<64 hex>`. When both are present they must agree. |
| `reference` | for every bound artifact | An OCI reference ending in `@<digest>`: `<host>[:<port>]/<repository>[:<tag>]@sha256:<64 hex>`. The tag is dropped when the image is named to the cluster. |
| `os`, `arch` | no | The platform of a native image, such as `linux` and `arm64`; `arch` may be `os/arch`. An artifact without a platform serves any architecture. |
| `imageIndex` | no | For a multi-architecture image: `children`, each with `platform` and `manifestDigest`, and optional `requiredPlatforms`. Not with `arch`. |
| `chartVersion` | no | The chart's version. The install records it and warns when the item's `release.yaml` pins another; the artifact decides. |
| `layout` | no | A directory holding an OCI image layout whose `index.json` lists `digest`, relative to the file or absolute. |

## Selection

An item asks for an artifact by name for the management nodes' architecture, `linux/amd64` or `linux/arm64`. Candidates are the artifact of that exact name and the ones named `<name>-amd64` or `<name>-arm64`. An artifact that names its platforms, natively or through its image index, is taken only when they include the architecture; one that names none counts as built for the architecture its own name's suffix says, else for any. The artifact of the exact name that covers the architecture wins; else the sibling named for it, or failing that any sibling that covers it; else the exact name that names no platform. An artifact present only for another architecture stops the install, naming both architectures. A per-architecture sibling of a bound name is not an unused artifact.

## Layouts

With a `layout`, nothing is pulled from the reference's host: the install reads a chart from the layout by digest and imports an image into the node's containerd under the reference's repository and digest, through the `CTR` command (`ctr`, `sudo ctr`, or `nerdctl exec -i <node> ctr` when the node is a container). The reference is the name the cluster asks for, so its host need not exist; `make local-stack-artifacts` uses `nvke.local`. A layout reaches the one containerd `CTR` talks to, so on a cluster with more than one node the check warns and the artifacts should come from a registry. Without a `layout`, the install pulls a chart from its reference with the Docker credential store, and the nodes pull the image.

## What the install writes

A render copies the file into the rendered tree as `stack-artifacts.json` with every `layout` absolute, beside `images.txt`, which lists each image the tree runs with its layout. After a successful install, `nvke-deploy.sh` keeps both, with the render's notes as `render.log` and a `status` file, under `$XDG_STATE_HOME/nvke/installs/<time>.<id>/`. A dry run or a failed install writes no record; `check-site.sh --diagnostics` collects the newest one.

## How the check reports it

`deploy/check-site.sh --stack-artifacts FILE` prints:

| Finding | Meaning |
|---|---|
| `PASS stack artifacts: FILE (N artifacts; source REF, declared Q); verify it with: nvkr release verify --reference REF` | The file has a `source` block. A `WARN` follows: the qualification is declared, not verified. |
| `PASS stack artifacts: FILE (N artifacts, no source block)` | The file names no source. |
| `FAIL stack artifacts: FILE does not parse: PROBLEM` | The first problem; the command it names prints every one. |
| `FAIL stack artifacts: layout for NAME is missing or does not list its digest` | A `layout` that is not there or holds another artifact. |
| `FAIL stack artifacts: ITEM ROLE NAME is not in the file`, `... is published only for ARCHS; the nodes are ARCH` | An image the deploy path binds that the file lacks for the nodes' architecture. |
| `INFO stack artifacts: NAME is not used by DEPLOY_PATH` | An artifact no item binds. |
| `WARN stack artifacts: N layout(s) and M nodes; ...; CTR is VALUE` | Layouts on a cluster with more than one node. |

The check also probes the registry hosts of the artifacts without a layout. When everything passes, the install command it prints carries `--stack-artifacts FILE` and, with layouts, the `CTR` it will import them with.
