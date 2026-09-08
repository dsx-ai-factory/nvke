<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# `runc.keyring`

The ASCII-armored OpenPGP public keys of the runc release signers, used to
verify runc's clearsigned `runc.sha256sum` checksum document before
`nke-kubeadm-distro-<x.y.z>` bundles the runc binary.

## Provenance

Copied byte for byte from the runc repository's `runc.keyring` at commit
[`68fbaabf63efc4ff2ed913c7df53d18c87be1432`](https://github.com/opencontainers/runc/blob/68fbaabf63efc4ff2ed913c7df53d18c87be1432/runc.keyring)
(`main` as of 2026-09-03):

```bash
curl -fsSL -o runc.keyring \
  https://raw.githubusercontent.com/opencontainers/runc/68fbaabf63efc4ff2ed913c7df53d18c87be1432/runc.keyring
```

sha256: `9bd9742cb79b215cf5f7f71ea71a51a2a95ab573412d54c2905be072d8fcab7c`

`bundle.sh` imports this file into a scratch `GNUPGHOME` created and destroyed
for that one verification, so the build host's own keys are never consulted.
