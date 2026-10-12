# nvke-agent Debian package

`packaging/nfpm.yaml` packages the agent and updater built from this checkout,
using the nfpm version pinned in `make/tool-versions.mk`. The Debian maintainer
scripts live beside the spec. The shared service units remain in `agent/scripts/`
because the release tarballs also ship them.

From the repository root, on a Linux host of the target architecture:

```sh
make -C agent deb-amd64
make -C agent deb-arm64
```

These targets build the binaries, check libc linkage, and invoke nfpm. Outputs
are `agent/bin/nvke-agent_<version>_<arch>.deb`. Set `DEB_OUTPUT` to choose an
exact output path, or `NFPM_BIN` to use an existing nfpm executable. Packer uses
the same targets and installs the result with dpkg.

Normal package installation enables and restarts the agent. Image baking sets
`NKE_AGENT_SKIP_START=1` to enable it without starting it before instance identity
exists. The image-specific SPIRE drop-in is installed separately and survives
package replacement.

The package replaces and provides `nke-agent`. Existing nodes normally adopt it
when reprovisioned from a new image. A manual `apt install nvke-agent` also works:
it moves persistent state, encryption checkpoints, retained upgrade artifacts,
and user scripts to the `nvke` directories. It preserves the environment file
and SPIRE service drop-ins. The old service, binary, environment, and state paths
remain aliases for one release, so old controller commands and rollback agents
can use the same files. The controller still owns `/etc/nke`; installation does
not move that directory.

Migration stops if old and new state trees both contain files, or if two
independent agent environment files exist. Resolve that conflict before retrying;
the package does not overwrite either copy.

Controller-side version selection remains in #1496. This change preserves the
legacy controller restart command and JWT audience, while the new agent accepts
both controller URI SANs, both SPIFFE naming forms, and both RBAC RPC names.
The agent supports `--controller-audience=nvke-controller` for #1496 to select
per node. It defaults to the legacy audience until the controller accepts the
renamed contract; changing that default alone would impose a release order.
