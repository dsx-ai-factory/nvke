# Validation record — 2026-11-16

## Environment

Fresh source fetched directly from GitHub at the revision in `versions.json`.
No existing source checkout, tenant or old generated test credentials were used.
Public dependency and image caches were available. The Linux Docker host was
Colima on macOS arm64, with 8 CPUs and 16 GiB RAM; its disk size was 150 GiB.

| Tool | Observed version |
|---|---|
| git | git version 2.54.0 (Apple Git-157) |
| python | Python 3.15.2 |
| go | go version go1.27.3 darwin/arm64 |
| make | GNU Make 4.4.1 |
| bash | GNU bash, version 5.3.20(1)-release (aarch64-apple-darwin25.6.0) |
| kind | kind v0.33.0 go1.27.0 darwin/arm64 |
| kubectl | v1.37.2 |
| helm | v4.3.1+gc0d08d0 |
| tilt | v0.37.8, built 2026-09-29 |
| docker | Client 29.9.3 / Server 29.5.2 |

## Successful full replay

The third independent fresh checkout completed the default path with exit code
zero in about 29 minutes. Only the documented inputs were supplied. The source,
three included patches and generated credentials were prepared afresh.

- Nine focused lifecycle cases passed, including the source suite's VIP-backed
  tenant creation, Kubernetes readiness, Mokka/operator checks and API deletion.
- All 19 unchanged mixed-demo checks passed. The single-control-plane mixed
  tenant used its control plane directly, without a VIP.
- GB300 advertised four GPUs; H100 advertised eight. Both workers ran kubelet
  `v1.36.4`. Each product-selected pod ran in the matching pool and saw one GPU.
- Both operator validators reported four init stages with exit code zero.
  These are the mock configuration's stages: driver/toolkit installation and
  the CUDA workload are disabled, so this does not prove real CUDA execution.
- Tenant deletion through NVKE passed; no tenant CAPI objects or node containers
  remained. Dedicated management, BGP, bundle mirror and volume cleanup passed.
- Docker/Kind inventory was independently checked after cleanup for all three
  attempts. Their observed tenant containers and named resources were absent;
  the pre-existing management cluster was preserved. Images/caches remain.

See [selected results](example-results.json). AICR installation, physical GPU
validation and scheduler changes were outside this replay. This directory
contains no private state or source/build output.

## Failure found while packaging

The first fresh run passed all nine focused lifecycle cases. Its separate mixed
tenant then failed during CNI installation: the agent tried to reach
`https://172.31.1.1:6443/version` before kube-vip was serving that endpoint.
Kube-vip started later, but the agent retained a CNI error. That attempt was
stopped and its tenant was deleted through the normal NVKE API, followed by
removal of its dedicated management cluster and BGP/bundle resources.

The helper was changed to omit `controlPlaneVIP` for its single-control-plane
mixed tenant. This uses NVKE's first-control-plane endpoint, removing the extra
VIP dependency from the GPU smoke test. No upstream controller/agent code was
changed. The focused suite still uses the VIP path. This is an explicit local
fixture adjustment, not a fix or qualification of VIP startup.

A second fresh run stopped before provider/controller deployment because the
public SPIRE archive repeatedly exceeded the upstream 30-second download limit.
A packaging patch increases that limit to 600 seconds and the retry time budget
to 1,200 seconds, preserving the exact artifact checksums. Cleanup was exercised
against this pre-controller failure and removed the empty management cluster.

## Offline checks

`python3 -m unittest -v test_guards.py` passed all four checks:
existing work is preserved; a bad patch is rejected before source retrieval;
cleanup refuses an unrecognised tenant before making API calls; and an empty
pre-controller cluster can be cleaned without requiring the tenant API.

The package was checked against this run's generated credentials and for
personal absolute paths. Private state and source/build output are excluded
from the distributable package.

## Qualification boundaries

Linux/amd64 and the optional `--keep-tenant` success path have not been exercised
in this validation. Downloaded artifacts and build caches are retained after
cleanup. See README for the simulation's functional limits.

Created with AI assistance.
