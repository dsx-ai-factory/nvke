# Profiles fixture

The profiles tree the controller E2E lanes deploy (`NVKE_CONTROLLER_PROFILES_DIR`; the deprecated `NKE_CONTROLLER_PROFILES_DIR` and `CWE_PROFILES_DIR` still work until [nvke#1399](https://github.com/dsx-ai-factory/nvke/issues/1399)), built into the
`cwe-profiles` image by the `Dockerfile` one directory up. The fleet profiles
live in `nke-site-deploy/cwe-profiles`; this tree is a fixture for tests.

Config profiles only. OS images and releases are `MachineImage`,
`ClusterRelease` and `NodePoolRelease` custom resources, and this repository's
set of those lives in `../releases/`.

`configProfiles/nico-vms-cp.yaml` and `nico-vms-worker.yaml` are copies of the
fleet config profiles the `nico-vms` specs name, and `../runbooks/` holds the
nine runbooks their hooks reference. The controller validates at startup that
every hook resolves to a script under `runbooks/`, so the two sets travel
together. The fleet files carry NVIDIA's proprietary header; the copies carry
this repository's Apache-2.0 header, as NVIDIA-owned fixture data.
