# Replay the mixed-GPU NVKE/Mokka baseline

This package starts from pinned NVKE source, creates a dedicated local CAPD
management cluster, runs the focused Mokka lifecycle test, then creates a new
tenant and runs Ben's mixed-model demo. It uses CPU containers, not real GPUs.

Expected result: **9 focused lifecycle cases and 19 mixed-model checks pass**.
One pool reports `NVIDIA-GB300-NVL` with four GPUs; another reports
`NVIDIA-H100-80GB-HBM3` with eight. Both validators pass and product-selected
pods run in the matching pools, seeing one assigned GPU each.

For the recorded run and its limits, see [VALIDATION.md](VALIDATION.md) and
[selected results](example-results.json). This directory packages a pinned
reference experiment; it does not run the current PR checkout as the controller.

## Prerequisites and inputs

- A Linux Docker host, or Docker in a Colima VM on macOS, with the same CPU
  architecture as the client (arm64 or amd64). The measured replay is on macOS
  arm64. Linux/amd64 is a supported path in the upstream runner, not a second
  platform qualification from this package.
- At least **8 CPUs, 16 GiB RAM and 100 GiB free Docker data disk**. Use a
  150 GiB disk for headroom. Start the VM yourself, for example
  `colima start --cpu 8 --memory 16 --disk 150`, or start an existing profile.
  The script does not resize, destroy or recreate your VM.
- Python 3.10+, Go **1.27.3**, GNU Make, Bash 4+, Git, Docker with Buildx,
  kind, kubectl, Helm, Tilt, jq, dpkg-deb, curl, OpenSSL, tar and sha256sum on
  PATH. On macOS, GNU Make may be installed as `gmake`; `REPLAY_MAKE` can name
  another GNU Make executable. Bash must resolve to version 4+ as `bash`.
  Use the pinned source's development/tool manifests for installation details.
- Git read access to `git@github.com:dsx-ai-factory/nvke.git`, and HTTPS access
  to the source project's public module, chart, image and release endpoints.
  Your normal Git access may be needed; **no NVIDIA cloud/tenant credentials**
  are embedded or required by this local experiment.
- Run CAPD tests serially. The Docker host's kernel, BGP port 179 and tenant
  VIP `172.31.1.1` are shared host resources. Do not run other tenant lifecycle
  tests concurrently. Existing clusters are not deleted by this package.
- On macOS the work directory must be in a location shared into Colima, such
  as your home directory. Space-containing paths have not been qualified.

Only two normal user inputs are needed: a fresh work directory and a reachable
local Docker socket. By default the socket comes from the active Docker
context; `DOCKER_HOST=unix:///path/to/docker.sock` explicitly overrides it.
Remote TCP/SSH Docker hosts are refused. Keep the shell/tool environment the
same for all commands. No Flox activation or migration is performed.

## Run from a fresh directory

From this directory (`hack/mokka-demo/replay`), run:

```sh
python3 replay.py prepare "$HOME/nvke-mokka-replay"
python3 replay.py doctor "$HOME/nvke-mokka-replay"
python3 replay.py run "$HOME/nvke-mokka-replay"
```

`prepare` refuses an existing directory. It fetches the exact commit in
`versions.json`, creates a local `test/mokka-replay` branch, checks the patch
hashes, and applies only the included scenario, macOS Helm and download-timeout patches.
It does not use an existing NVKE checkout, an old tenant or previously saved
local-test credentials. An alternate Git transport or local repository mirror
can be supplied explicitly with `prepare --repository URL`; the commit remains
pinned. Do not place credentials in repository URLs.

The full run can take tens of minutes, especially without image/module caches.
Public dependency caches may be reused. On failure it exits nonzero and leaves
private diagnostics (`focused.log`, `mixed-helper.log` and `mixed.log`) for inspection; do not treat a partial setup as a pass.
`run` refuses to overwrite a prior run. Use a new directory for a new attempt.

By default both disposable tenants are deleted after their tests. The dedicated
management cluster stays up until the cleanup command below. To keep the
successful mixed tenant for a follow-up runtime experiment, use:

```sh
python3 replay.py run "$HOME/nvke-mokka-replay" --keep-tenant
```

Choose the default or retained-tenant form for a given new work directory, not
both. Failure does not intentionally retain the mixed tenant. The run prints
its retained tenant name and kubeconfig path. That kubeconfig is private.

## What runs where

1. GNU Make builds the matching `nvkectl` from the fresh checkout.
2. The repository's `controller` target `test-e2e-capd-local` builds the local
   node agent/CAPD image, installs management services through Tilt, and runs
   the nine `mokka`-selected create/readiness/operator/workload/delete checks.
3. `mixed.py` uses the pinned repository's CAPD request fixtures and real local
   NVKE API to create another tenant with a control plane and initial worker.
   The mixed tenant uses its single control plane directly, without a VIP;
   the focused lifecycle suite separately exercises the VIP path.
4. Ben's unchanged mixed demo adds the two GPU-profile pools through `nvkectl`,
   installs one Mokka release per profile and one shared GPU Operator, checks
   labels/counts/validators, and runs product-selected one-GPU pods.
5. The helper records selected results and deletes the tenant through NVKE,
   checking that its CAPI resources and node containers are gone.

The API client mints a short-lived token for the dedicated management cluster
and uses a loopback port-forward. Tenant API access verifies its certificate;
no TLS bypass is used. Commands use dedicated kubeconfigs and CLI configuration.

## Pins and recorded adjustments

`versions.json` is the source of truth. NVKE is pinned at
`6f2ff82bc5df2f39eb0ed0ac0fc82394940012c0` (Ben's #2106 base), with only the
mixed-scenario commit `289c0ff693da240202061b5c54dfa41ff47b102d` from #2109.
Mokka is 0.6.0 and GPU Operator is v26.3.3. The full older #2109 branch is not
applied. The other tool/container pins come from that exact source checkout.

- Public bootstrap downloads allow ten minutes per attempt instead of the
  upstream thirty seconds, retaining the same pinned SHA-256 verification.
- The macOS patch extracts pinned **Linux Helm** from the fixture's `.deb`
  instead of copying a host Mach-O binary into the Linux E2E client.
- Isolated anonymous Docker/Helm registry config avoids Keychain helper stalls
  for public artifacts, preserving the user's saved credentials/config.
- Join-token SPIRE attestation and `INSTALL_CNI_AFTER_INIT=false` explicitly
  select the exercised CAPD/public-bundle path. An owned, uniquely named bundle
  mirror serves the tenant's bootstrap artifacts. This is not TPM attestation
  or a production installation recipe.
- The mixed tenant omits `controlPlaneVIP`. A cold kube-vip startup raced the
  agent's immediate CNI installation in the first packaging test, leaving CNI
  installation in an error state. Using the first control plane directly
  avoids that extra dependency for this single-control-plane GPU smoke. The
  focused suite retains the source's VIP configuration. This package does not
  fix or qualify the upstream VIP startup sequence.
- Each run creates fresh management/tenant state. Cluster, BGP, bundle-volume
  and node-image names are recorded per run in `replay.json`.
- Requested and actual Kubernetes versions can differ in this container
  fixture; actual kubelet versions are captured in results. No Kubernetes,
  node-image-content or driver upgrade is qualified here.

## Results, private state and cleanup

`results.json` contains selected pass counts, product/capacity, validators,
actual kubelet versions and test PASS lines. Review that file before sharing.
Everything under `private/` must stay private: it contains generated local test
passwords, kubeconfigs, token caches and logs which may print those values.
Do not upload the whole work directory. `replay.json` records owned resource
names; `prepare.log` records source retrieval. This package itself has no secrets.

Cleanup uses the normal tenant API deletion path, refuses unrecognised tenant
names, checks CAPI resources, then removes only this run's management cluster,
BGP container and bundle mirror/volume:

```sh
python3 replay.py cleanup "$HOME/nvke-mokka-replay"
```

If bootstrap failed before any tenant CAPI resources existed, cleanup removes
the empty dedicated management cluster directly. If tenant resources exist but
the controller is unreachable, it stops and preserves them for inspection.
Inspect `private/focused.log` and `private/focused/tilt.log`, repair that local
controller failure and retry cleanup. No global prune or VM deletion is performed.
Downloaded images, build caches and private diagnostic files are retained.

## Boundaries

This replays provisioning, simulated NVML discovery, whole-GPU counts, operator
validation and product-directed placement. Driver/toolkit installation and the
CUDA validator workload are disabled. It does not prove physical driver/CUDA
compatibility, AICR installation, per-pool driver versions, NVSentinel recycling,
gang scheduling, training, networking or performance. Same-profile mocks may
repeat GPU UUIDs across nodes. Untested customer combinations are not blocked
by an allowlist; this is one documented, pinned reference experiment.

Included upstream patches retain their Apache-2.0 notices; see LICENSE and
the linked source commits for provenance.

Created with AI assistance
