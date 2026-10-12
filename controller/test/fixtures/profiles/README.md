# Profiles fixture

The profiles tree the CWE E2E lanes deploy (`NKE_CONTROLLER_PROFILES_DIR`; the deprecated `CWE_PROFILES_DIR` still works), built into the
`cwe-profiles` image by the `Dockerfile` one directory up. The fleet profiles
live in `nke-site-deploy/cwe-profiles`; this tree is a fixture for tests.

Moving OS profiles into `MachineImage`, `ClusterRelease` and `NodePoolRelease`
custom resources is in progress. It has not happened yet: CWE still reads the OS
profiles from this tree. `../releases/` holds the resources generated from it
with `make fixture-releases`, ready for when CWE starts reading them; a Go test
fails when the two drift. Until then, treat this tree as the source of truth and
`../releases/` as generated output that nothing consumes yet.

Two kinds of entry live here, and the difference matters:

| Profile | URLs | Boots |
|---|---|---|
| `test-fixture-amd64`, `nvke-local-dev-kindest-arm64` | `example.test`, synthetic | nothing; they exercise CWE's rendering and the CAPD and gofail lanes |
| `nvke-nico-vms-24.04-amd64-fde`, `nvke-nico-vms-24.04-arm64-fde` | unreachable placeholders | the NVKE node image on the `nico-vms` backend |

**No file in this tree names a CDN host.** These profiles used to point
`image-url` at CloudFront and pass `--bundle-server-url` for a CloudFront
bundle server. The self-hosted arm64 fleet gets 403 from that CDN, so the lane
now pulls the node image from GHCR and rewrites `image-url` to the local copy,
and the agent runs with no bundle server, which makes it prefer apt against the
`nke-cni-*` debs already in the node image. `nico-vms-e2e.sh` fails the run if
any file here names CloudFront again.

`nvke-nico-vms-24.04-amd64-fde` is a copy of the fleet profile `nke-nico-vms-24.04-amd64-fde`.
It is the OS profile every `nico-vms` E2E spec requests, and CWE reads its
`ipxeScript` and `userData.tmpl` unconditionally when it creates a node, so
without the copy the first create fails. The files hold no credentials: the
image is fetched with a plain GET, and the one `token_name` line is a template
variable. When the fleet profile changes, copy it again; nothing syncs it.

`configProfiles/nico-vms-cp.yaml` and `nico-vms-worker.yaml` are copies of the
fleet config profiles the same specs name, and `../runbooks/` holds the eight
runbooks their hooks reference. CWE validates at startup that every hook
resolves to a script under `runbooks/`, so the two sets travel together. The
fleet files carry NVIDIA's proprietary header; the copies carry this
repository's Apache-2.0 header, as NVIDIA-owned fixture data.
