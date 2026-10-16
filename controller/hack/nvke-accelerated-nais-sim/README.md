# NVKE Accelerated NAIS Integration Simulator

The NVKE Accelerated NAIS Integration Simulator runs VMaaS's NVIDIA OVN
1.5.18 DPU profiles with simulated VFs.
NKE runs in a management cluster and provisions both the DPU cluster and the
VMaaS secondary cluster through `nico-sim`. The lab uses real Multus, OVN CNI,
DPU representor programming and FRR routing. The simulation orchestrator
(`managed.py`) coordinates provisioning, simulated device wiring and validation.

The released NVIDIA image needs the simulation-only fixes in
[`patches/nvidia-ovn-simulation.patch`](patches/nvidia-ovn-simulation.patch).
`prepare.py` builds a clearly named child image from the exact NVIDIA source
and base image. This qualifies that patched simulation image, not an
unmodified production release or BlueField hardware.

The management cluster provisions both tenant clusters; each secondary-cluster
host is paired with one node in the DPU cluster.

```text
       management Kind cluster: NKE + PostgreSQL + Temporal + CAPI/CAPNICo
                                |
                         nico-sim NICo API
                                |
                   creates both clusters' nodes
                                |
       secondary Kubernetes             DPU-service Kubernetes
       kubeadm, no kube-proxy           kubeadm, OVN primary CNI

       CP host + pod A <==== VF veths ====> DPU A: OVN + OVS + FRR
       worker + pod B <===== VF veths ====> DPU B: OVN + OVS + FRR
                                                          |
                                                  FRR fabric router
                                                          |
                                                 external HTTP endpoint

       DPU OVN and FRR controllers --> secondary Kubernetes API
       pod A --> VF --> DPU A --> fabric router --> DPU B --> VF --> pod B
```

OVS runs on the DPU fixtures. The compute hosts retain their management IP
and default route on `eth0`. Each pair has 16 simulated VF channels:
`eth0-0` is the gateway VF, indices 1–2 are management VFs, index 3 is the
reserved host uplink, and indices 4–15 are pod devices. The fixture provides
separate management, gateway, uplink and external Docker networks.

## Run with NKE provisioning both clusters

The default Docker context is `colima`, using the existing default VM.
`prepare.py --managed` and `managed.py create` accept `--docker-context`.
The management Kind cluster contains NKE, PostgreSQL, Temporal, CAPI and the
real CAPNICo provider. Each tenant cluster has one control-plane node and one
worker. The DPU cluster requests `ovn-kubernetes`; the secondary cluster requests
`ovn-kubernetes-dpu-host` with version `1.5.18`. NKE labels the secondary nodes
before kubeadm init/join, skips host OVS and kube-proxy, and installs the
packaged DPU-host profile through the node agent.
Both use version `1.5.18`. Full OVN owns ordinary DPU-cluster pods and Services.
VMaaS DPU OVN and FRR run in the separate `vmaas-offload` network namespace,
using private OVS/OVN sockets and databases. Their Kubernetes resources live in
`ovn-kubernetes-vmaas`, leaving primary OVN's resources in `ovn-kubernetes`.
The pinned NVIDIA image lacks the named-instance settings used in the internal
NKE+DPF guide; namespace isolation supplies that separation for this software lab.
The DPU pod CIDR is `10.236.64.0/18`; secondary pods use `10.234.64.0/18`.

Requirements: Linux arm64 Docker runtime with kernel Open vSwitch available,
Kind, kubectl, Helm, skopeo, Go 1.26+, make, Python 3.12+ and authenticated `glab`
access to the NVIDIA input projects. On macOS, Docker/Colima supplies Linux;
namespace operations run in a privileged driver container. Allow approximately
6 GiB free runtime memory and several GiB of disk space in addition to other
labs. Fixed fixture subnets `172.28.234.0/24`, `172.28.235.0/24` and
`172.28.236.0/24` permit one traffic lab at a time on a Docker daemon.

Use the existing NKE node image `cwe-capd-kindest-node:local`. If absent, build
it with `controller/hack/capd-kindest-node/build-image.sh`; that existing helper
also assembles the agent bundle.

Set up the Python environment and prepare the pinned networking inputs:

```sh
python3 -m venv controller/hack/nvke-accelerated-nais-sim/.state/venv
controller/hack/nvke-accelerated-nais-sim/.state/venv/bin/pip install -r controller/hack/nvke-accelerated-nais-sim/requirements.txt
source controller/hack/nvke-accelerated-nais-sim/.state/venv/bin/activate
make -C controller prepare-nvke-accelerated-nais-sim \
  OVN_DPU_CHART=/path/to/ovn-kubernetes-1.5.18.tgz
```

The managed preparation command builds the NKE agent, controller and simulator
through their normal Make targets. It consumes the Linux arm64 CAPNICo image
`cluster-api-provider-nico:local`, built through CAPNICo's own workflow and
available in the selected Docker context. In a CAPNICo checkout, its standard
image target is `make docker-build IMG=cluster-api-provider-nico:local`.
The lab does not build or repackage CAPNICo from a manually supplied binary.
The remaining preparation stages the pinned VMaaS chart, simulation overrides
and helper images. These are local lab fixtures; they do not qualify production
OS packaging or cloud-init provisioning.

Build and start the NKE-managed simulator. `CAPNICO_CHART` must match the CAPNICo
image. Preparation uses the same static CGO simulator build as the NICo E2E
lane; the build runs inside Linux for SQLite support.

```sh
make -C controller prepare-nvke-accelerated-nais-sim-managed \
  OVN_DPU_CHART=/path/to/ovn-kubernetes-1.5.18.tgz
make -C controller test-nvke-accelerated-nais-sim-managed \
  OVN_DPU_CHART=/path/to/ovn-kubernetes-1.5.18.tgz \
  CAPNICO_CHART=/path/to/cluster-api-provider-nico/chart
```

The run prints its private `.state/managed-<run>/` directory and retains it
on failure. A successful run writes `management-validation.json`, traffic
evidence and three kubeconfigs, and updates `.state/managed-latest`.
Use `kubectl --kubeconfig <state>/vmaas.kubeconfig` or `dpu.kubeconfig` to inspect
the clusters. API proxies listen on random loopback ports.

```sh
python3 controller/hack/nvke-accelerated-nais-sim/managed.py verify --state /path/to/run
python3 controller/hack/nvke-accelerated-nais-sim/managed.py scale --state /path/to/run --workers 2
python3 controller/hack/nvke-accelerated-nais-sim/managed.py restart --state /path/to/run
python3 controller/hack/nvke-accelerated-nais-sim/managed.py install --state /path/to/run
python3 controller/hack/nvke-accelerated-nais-sim/managed.py cleanup --state /path/to/run
```

`resume --state /path/to/run` continues provisioning an existing managed run.
If management bootstrap is incomplete, also pass `--provider-chart`.
`test-nvke-accelerated-nais-sim-traffic` is an alias for
`test-nvke-accelerated-nais-sim-managed` and requires the same chart inputs. `networking.py` supplies fixtures and assertions; it has no
standalone provisioning entry point.

`scale` increases capacity through the NKE node-pool API, prepares the added
DPU fixture, and then adds a secondary worker. Host/DPU pairing and fixture
addresses remain stable. Secondary workers request `nodeType=gpu`; verification
checks that label and `k8s.ovn.org/dpu-host` in kubelet's bootstrap arguments,
IPv6, schedulability, preserved management networking and absence of host OVS
bridges. The traffic matrix covers every node: 38 checks with two nodes per
cluster, 82 with three, and 144 with four. Simulated VF wiring remains a lab
fixture; this does not validate physical VF creation, VFIO binding or cloud-init
execution.

Cleanup requests tenant-cluster deletion through NKE before removing this run's
management cluster and labeled fixtures. It preserves other default-VM labs.
This local lane disables mTLS and uses the existing
`LOCAL_DEV_FORCE_DOCKER_MACHINE_BOOTSTRAP` fixture switch because container
nodes do not run cloud-init. Runtime database credentials are generated into
a Kubernetes Secret. SPIFFE/OIDC, physical DPUs, HA and full VMaaS application
deployment require separate qualification.

At the initial two-node size, verification runs 38 network checks across both clusters:
same-node and cross-node pod HTTP, pod-to-node and node-to-pod traffic in both
directions, node-to-node management traffic, secondary-node gateway VF traffic,
node-to-Service access, pod DNS and authenticated Kubernetes API Service access.
`network-matrix.json` records the actual endpoints and observed source IPs.
The traffic assertions also capture the secondary-cluster fabric
path and check unencapsulated pod TCP, external traffic and outbound SNAT.

FRR bootstrap admission uses its host-network HTTPS endpoint with an IP SAN
certificate, because its FRRConfiguration must exist before OVN creates the
advertised network and provides ClusterIP forwarding. Admission remains `Fail`
and verifies TLS. `install` renews the lab's 24-hour certificate and 8-hour
ServiceAccount tokens. The fixture supplies VF resource limits to both CoreDNS
and NKE's pod-network DNS-cache DaemonSet, and grants the chart service account
finalizer updates scoped to `vm-uplink` for UplinkState ownership.
VF MAC initialization runs once, preserving OVN's later MAC and neighbor setup.
The node image persists IPv6 enablement for `all`, `default` and `lo`; the
offload namespace enables the same settings for FRR's sockets/link-local use.
IPv6 forwarding and dual-stack pod networking are not required by that check.

See [VALIDATION.md](VALIDATION.md) for observed NKE lifecycle and traffic
evidence from the default VM.

Runtime kubeconfigs, short-lived ServiceAccount tokens, source archives,
image archives and logs stay under ignored `.state/` with a private directory.
The DPU and FRR daemons use scoped secondary-cluster ServiceAccounts; `install`
renews their 8-hour credentials. SPIFFE exchange and automatic token rotation
are separate work.

## Inputs and simulation adaptations

| Input | Pin |
| --- | --- |
| NVIDIA OVN chart | `ovn-kubernetes:1.5.18` |
| Chart SHA256 | `470e8255ce3f4a7362b84471d4d183757f6b679a94ea882500feac5a6abd75f2` |
| VMaaS base image | `nvcr.io/fcypcg1knhby/vmaas-dev/ovnkube:a8b97898-69369865-2b600cc` |
| NVIDIA source | `a8b978986e8903f883a006808074bde83596139c` |
| Simulation child | `nke-vmaas-ovn-sim:a8b97898` |
| Device plugin source | `a99c4111eb1b3db7241f666662906f2cc8b0e42a` |
| FRR-K8s source | `b43efcb206be36a361401a233908b74b8e8b077e` |
| Host and DPU Kubernetes | `v1.34.8` |

Preparation obtains the NVIDIA arm64 image from the published CI Debian
artifact in GitLab project 321312, job 459788815. Its SHA256 is
`d660cec5f0817d56bb9bb51aeac87612b0457410fbd46fed76bc51f18be6ba3f`.
The command verifies the resulting image identity. It can also accept an
existing package with `prepare.py --image-package /path/to/package.deb`.
FRR-K8s is built from the pinned source as `nke-dpu-frr-k8s:b43efcb2`; the old
upstream development-image digest was removed. FRR and Multus runtime images
are pinned by digest in the preparation source.
The public helper sources are pinned; OVN runtime and source are NVIDIA's.

The lab layers `values-host-sim.yaml` and `values-dpu-sim.yaml` over the chart's
`values-images.yaml`, `values-vmaas-nooverlay-dpu-host.yaml` and
`values-vmaas-nooverlay-dpu.yaml`. It preserves their no-overlay and outbound
SNAT settings. The simulation uses kernel OVS (`system`) because veths cannot
provide the production `netdev`/DPDK representor datapath.

The chart omits the DPU simulation environment variable and host gateway VF
option. The fixture supplies them, validates the OVS host pairing in place of
BlueField sysfs initialization, and reduces debug log volume. The source patch
avoids physical PF MAC/PCI and devlink/rate-limit calls in simulation, accepts
only connection details for the paired host, and resolves renamed management
VFs through their interface aliases. Physical DPU mode retains the PF MAC gate.

FRR-K8s configuration lives in the workload cluster while its daemons run on
the paired DPUs. The chart's default-network RouteAdvertisements selects the
fabric configuration. The fabric routes host management traffic through each host gateway VF
to keep API and webhook return paths symmetric. A single-CP lab disables
Kubernetes controller/scheduler leader election to avoid restart churn under
shared Docker VM pressure; this does not qualify HA behavior.

## Scope

The lab uses NKE REST, its transactional outbox, Temporal, CAPI,
CAPNICo and the node agent to provision both clusters. Auxiliary device-plugin,
Multus, DPU-service and fabric configuration remains lab-owned.

The assertions cover default-network traffic, BGP, source preservation, SNAT,
DNS, HTTP ClusterIP, authenticated API Service access and unchanged host
management networking. Fabric packet capture prevents overlay connectivity
from being mistaken for a successful routed pod path. Whole-container reboot,
node replacement, tenant CUDN/VRFs, DPF lifecycle, hardware offload, DPDK and
production identity rotation are not qualified by this lane.

See [VALIDATION.md](VALIDATION.md) for observed results.
