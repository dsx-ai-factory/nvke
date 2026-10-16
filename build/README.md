# build

Scripts that build NVKE artifacts from this checkout.

| Script | What it builds |
|--------|---------------|
| `agent.sh` | The `nvke-agent` Debian package and container image |
| `stack-artifacts.sh` | The images and charts `deploy/nico` installs, as OCI layouts, and the `stack-artifacts.json` that names them. `make local-stack-artifacts` runs it |

## stack-artifacts.sh

```bash
make local-stack-artifacts            # writes dist/local-site/stack-artifacts.json and dist/local-site/images/<name>/
make local-stack-artifacts ARCH=arm64 CONTAINER_CLI=nerdctl OUT=/srv/nvke/artifacts
```

Five artifacts: the controller image (`cwe`), the profiles image built from `controller/test/fixtures` (`controller-profiles`), a copy of the CAPNICo image `deploy/nico/capnico/release.yaml` pins (`capnico-controller`), and the controller and NKTP charts packaged from the tree (`controller-chart`, `nktp`). The NVKE-built artifacts are named `nvke.local/<name>@<digest>`; the install imports each layout into the node's containerd under that name. The third-party charts come from their public repositories at install time.

Needs `docker` with buildx or `nerdctl`, `helm`, `oras`, `python3` and `nvkectl` from `make -C cli build`, or `NVKECTL=<path>`. The build goes to a temporary directory and replaces the output only once it is complete and loads, so a failed build leaves the previous one as it was. [docs/operate/get-artifacts.md](../docs/operate/get-artifacts.md) is the operator's page; [cli/docs/stack-artifacts.md](../cli/docs/stack-artifacts.md) the file's reference.

## Related docs

- [Architecture](../docs/architecture.md)
- [Node OS Image](../os-image/README.md)
- [node-agent](../agent/README.md)
