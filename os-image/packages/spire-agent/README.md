<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# spire-agent

Builds the Debian package that gives a node its SPIFFE identity:

| Path | Contents |
|---|---|
| `/usr/local/bin/spire-agent` | The upstream SPIRE agent release binary (musl) |
| `/usr/local/lib/spire-agent/tpm_attestor_agent` | spire-tpm-plugin's agent-side node attestor |
| `/usr/local/bin/get_tpm_pubhash` | spire-tpm-plugin's EK-reading helper |
| `/lib/systemd/system/spire-agent.service` | The agent unit |
| `/lib/systemd/system/nvke-spire-configure.service` | Renders the config before it |
| `/usr/lib/nvke/spire-configure.sh` | What that unit runs |
| `/usr/lib/nvke/spire-agent.conf.tmpl` | The template it renders |

`provision.sh` installs this package at bake time — it is not staged for
`nvke-agent` to install later, the way the CNI and kubeadm-distro packages are.
SPIRE is how a node gets an identity at all, so there is no point in the image
at which it is optional.

All four unit/script files are read straight out of [`../../files/`](../../files/),
where the image's copies have always lived; the package takes ownership of them
rather than forking a second copy. They ship together because
`spire-agent.service` has `Requires=nvke-spire-configure.service` — splitting
them would leave a broken unit on any node that installs this package on its
own.

`postinst` does what a file list cannot: creates the `spire` system user, adds
it to `tss` so the attestor can open `/dev/tpmrm0`, creates
`/var/lib/spire-agent` owned by that user, and enables both units. It also
removes a stale `/etc/systemd/system/spire-agent.service`, which images built
before this package existed have from `provision.sh` and which would otherwise
mask the vendor unit in `/lib`.

## Why this package exists

Two divergent install paths used to exist: the Packer build installed raw
upstream SPIRE and TPM plugin binaries, while apt-path nodes installed
a `spire-agent` deb built out of an internal GitLab repository and served from
the NVKE S3 apt repo. This package is the single artifact both converge on, and
every byte it wraps comes from a public GitHub release.

## Verification

`spire-agent` is verified twice: against the digest SPIRE publishes in the
`<asset>_sha256sum.txt` sidecar beside each release asset, and against the
committed lock.

**spire-tpm-plugin publishes no checksum document of any kind**, so for
`tpm_attestor_agent` and `get_tpm_pubhash` the lock is the only pin. Treat a
lock diff on those two rows as a supply-chain event and confirm the change is
an intended version bump before committing it.

## Building

```bash
make debs OUT_DIR=/absolute/path          # amd64
make debs OUT_DIR=/absolute/path ARCH=arm64
```

Linux only. `debs` runs `bundle` first, which verifies every artifact against
`lock/<spire>+tpm<tpm>.lock` before anything is extracted, and reindexes
`OUT_DIR` with `dpkg-scanpackages` when it finishes. `make repro-check` builds
twice and compares every `.deb` byte-for-byte.

`make test` exercises a fresh bundle for both architectures using local
release archives. It needs Python 3, Bash, curl, tar, awk, and GNU coreutils.

## Bumping the versions

Set `SPIRE_VERSION` and/or `TPM_PLUGIN_VERSION` in `make/tool-versions.mk` at
the repository root, refresh the lock, review it, commit it:

```bash
make lock
git diff -- lock
```

`make lock` resolves **both** architectures in one pass, so the lock never
carries a half-updated version pair.

⚠️ `TPM_PLUGIN_VERSION` also pins the **server**-side attestor elsewhere in
this repository:

- `controller/Tiltfile` — the `tpm_attestor_server` image digest
- `controller/hack/ci/prepare-capd-bundles.sh`
- `cli/internal/bootstrap/manifest/bootstrap-manifest.yaml` — the `tpm-attestor` image
- `cli/internal/bootstrap/controllerdeploy/controllerdeploy.go` — the tpm-direct plugin checksums the bootstrap enforces
- `cli/e2e/bootstrap/substrate/image.go` — `get_tpm_pubhash` for the e2e substrate

The agent and server halves of the attestor must move together. Bump them in
the same change.
