# nke-agent Debian package

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
are `agent/bin/nke-agent_<version>_<arch>.deb`. Set `DEB_OUTPUT` to choose an
exact output path, or `NFPM_BIN` to use an existing nfpm executable. Packer uses
the same targets and installs the result with dpkg.

Normal package installation enables and restarts the agent. Image baking sets
`NKE_AGENT_SKIP_START=1` to enable it without starting it before instance identity
exists. The image-specific SPIRE drop-in is installed separately and survives
package replacement.
