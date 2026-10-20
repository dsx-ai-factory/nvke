# validate/

`nvke-image-validate` checks a built NVKE node image against its release
evidence, entirely offline, in three inspection tiers behind one versioned
check registry:

- **static** — no image open: the image's digest against the release's
  attested `checksums.txt` (verified against a pinned sigstore trusted root),
  artifact-name identity, and the target BOM against the release SBOM.
- **guestfs** — read-only filesystem facts via libguestfs: installed packages
  vs the SBOM, masked units, etcd directory ownership, kernel flavor, LUKS
  presence. Opens FDE images with `--luks-passphrase-file`.
- **boot** — behavioral assertions from inside a throwaway QEMU boot
  (`--boot on`): unit enablement and ordering as systemd resolves them,
  runtime sysctl values, booted component versions.

```console
$ nvke-image-validate \
    --image nvke-node-fde-ubuntu-26.04.1-amd64.qcow2 \
    --bundle release-evidence/ \
    --checks checks.yaml \
    --target-variant fde --target-arch amd64 --target-ubuntu 26.04.1 \
    --target-bom target-bom.yaml
nvke-node-fde-ubuntu-26.04.1-amd64.qcow2 (sha256:49647c…)
check set 0.2.0
  PASS   INT-001      sha256 49647c… matches release checksums entry …
  PASS   INT-002      attestation verified; signer https://github.com/…
  PASS   COMPAT-001   name declares fde/amd64/ubuntu-26.04.1, matching the target
  PASS   COMPAT-002   all 2 BOM components are in the release SBOM at required versions
  …
pass=11 fail=0 (blocking=0 warn=0) skip=4 error=0
```

`--bundle` points at the release evidence directory (`checksums.txt`,
`checksums.txt.sigstore.json`, `trusted-root.json`, per-image `*.spdx.json`);
`--checksums` is the digest-only fallback, which reports the unverified
evidence as a visible skip. `--target-bom` names the component versions the
management-plane target expects (`components: {name: version}`); without it
the BOM-driven COMPAT checks report a skip.

Exit codes: `0` pass, `1` a blocking check failed, `2` the run could not
evaluate (error always outranks fail). Warn-severity failures are reported and
counted without failing the run. `--format json` emits the schema-v1 report
for pipelines.

`--emit-verdict verdict.json --sign-key key.pem` additionally signs the run's
result as a DSSE in-toto attestation over the image's digest (predicate
`https://nvke.nvidia.com/image-validation/v1`; Ed25519 or ECDSA P-256 key). A
failing run still emits its verdict — the signed outcome is the evidence.
Downstream gates re-check the verdict in milliseconds instead of ever
re-reading the image:

```console
$ nvke-verdict --attestation verdict.json --key verdict.pub \
    --image nvke-node-fde-ubuntu-26.04.1-amd64.qcow2 --max-age 720h
verified: nvke-node-fde-ubuntu-26.04.1-amd64.qcow2 sha256:49647c…
  outcome: pass
  …
```

`nvke-verdict` exits 0 (authentic and passing), 1 (authentic, not passing) or
2 (could not verify — never read as a pass). In-process consumers use
`pkg/verify`, which is self-contained by design. The envelopes are standard:
cosign verifies them (`hack/cross-verify-cosign.sh`).

A check that cannot run never silently passes: an entry with no bound
implementation is an error, and a tier that is off (or missing its inputs)
reports a skip the report shows.

See [AGENTS.md](AGENTS.md) for layout, constraints and verification targets,
and [docs/decisions.md](docs/decisions.md) for the design decisions and their
reasons.
