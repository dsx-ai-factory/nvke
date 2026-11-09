# Add a Kubernetes minor version

NVIDIA Kubernetes Engine (NVKE) supports one Kubernetes version and stages a few
others on its node image. [Supported platforms and versions](overview/supported-platforms.md)
lists them. This page lists where a version lives in each component, and the
order of the changes for a new minor version.

## Where the version lives

| Component | What to change |
|---|---|
| Node OS (`os-image/`) | The node packages: follow "Bumping versions" in [`os-image/packages/kubeadm-distro/README.md`](../os-image/packages/kubeadm-distro/README.md) |
| Controller (`controller/`) | The kubeadm configuration NVKE renders, in `internal/kubeadmconfig/`, only if the new kubeadm rejects it |
| | The dev and test defaults: the Tiltfiles, `hack/create-*.sh`, the local test node image in `hack/capd-kindest-node/`, `test/e2e/` and `test/fixtures/releases/` |
| Node agent (`agent/`) | The kubeadm samples in `samples/kubeadm-deploy/`, which use the supported version |
| CLI (`cli/`) | The generated site spec default in `internal/bootstrap/sitespec/template.yaml`, and `examples/` |
| Deploy and management engine | `deploy/examples/local-site/` and `management-engine/config/dev/` |
| Release (`release/`) | The release qualification streams in `policy.yaml` |
| Docs (`docs/`) | [Supported platforms and versions](overview/supported-platforms.md) and [Prepare nodes](operate/prepare-nodes.md) |
| NVIDIA Kubernetes Recipes (NVKR) | The node image build in [`dsx-ai-factory/nvkr`](https://github.com/dsx-ai-factory/nvkr), from an NVKE release |

## Order of the changes

Each step is its own pull request.

1. **Stage the new minor.** Read its [changelog](https://github.com/kubernetes/kubernetes/tree/master/CHANGELOG)
   for removed APIs, removed feature gates and changed defaults. Add the node
   packages and keep the existing versions; dropping the oldest is step 3. List
   the version as staged in the two docs pages. If the new
   kubeadm rejects the configuration NVKE renders, change that in its own pull
   request. Test it as described in [Test the new version](#test-the-new-version).
2. **Make it the supported version.** Move the controller, agent sample, CLI,
   deploy and management engine defaults, the release streams and the docs.
3. **Drop the oldest version.** Remove it from `K8S_VERSIONS`, its lock files,
   the package tests and the docs, after nothing else uses it. For example,
   `controller/hack/ci/prepare-ovn-assets.sh` reads one version's lock file by
   name.

## Test the new version

Run `make preflight`, as described in [Development](development.md), for every
change. Then test on your machine with these methods. At least one of them
must use the new version: the kubeadm check or the Tilt with CAPD upgrade.

| Method | What it checks | How |
|---|---|---|
| kubeadm | The new kubeadm accepts the configuration NVKE renders and the samples in `agent/samples/kubeadm-deploy/` | [`kubeadm config validate`](https://kubernetes.io/docs/reference/setup-tools/kubeadm/kubeadm-config/) with the new kubeadm binary |
| Tilt with Cluster API Docker (CAPD) | A workload cluster created on the supported version, the default, with its control plane upgraded to the new one | "CAPI + Tilt Development (Recommended)" in [`controller/README.md`](../controller/README.md), then `WAIT=true TARGET_VERSION=<NEW_VERSION> ./hack/capd-upgrade.sh` from `controller/`, which returns when the control-plane upgrade finishes. The script builds the node image from `kindest/node:v<NEW_VERSION>`, so pick a patch that kind publishes; it can differ from the staged one. |
| CAPD end-to-end suite | That the change does not break the supported version, which the suite runs on | `make test-e2e-capd-local` in `controller/`, as described in [`controller/test/e2e/README.md`](../controller/test/e2e/README.md) |
| nico-sim VMs | Nodes as virtual machines, closer to real machines | [`controller/nico-sim/README.md`](../controller/nico-sim/README.md) |
| Local site | The operator flow, from site bootstrap to a workload cluster | [Run NVKE end to end on one machine](get-started/local-site.md) |

Optional checks on a local workload cluster:

- Upgrade only the control plane, and check that a worker node pool on the
  previous minor version stays `Ready`.
- Check the nodes' kubelet configuration and conditions.
- Check pod networking, Services and DNS.
