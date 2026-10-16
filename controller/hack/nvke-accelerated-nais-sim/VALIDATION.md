# NVKE Accelerated NAIS Integration Simulator validation

Observed on 2026-10-15 in the default Colima VM, Docker context `colima`.
Current run: `nvke-dpu-1df71542`.

The old `nvke-dpu-3a776d80` lab and the failed intermediate
`nvke-dpu-2b0cfe0e` run were removed through NKE teardown. Their containers
and networks are gone. One stopped container from an earlier DPU run was
also removed. All 11 unrelated Colima containers kept their IDs and remained
running. Prepared images and private evidence were retained.

## Results

The replacement management cluster ran NKE, PostgreSQL, Temporal and real
CAPI/CAPNICo. Both tenant clusters were created through NKE and the NICo
container backend. Initial creation completed without a resume or manual
node repair. All six initial provisioning workflows completed, and all
38 initial network checks passed.

Worker capacity was then increased through the NKE node-pool API:

- One to two workers per tenant: 82 network checks passed after fixing the
  lab's webhook placement. Its original Node watch replayed a snapshot, so
  that watch is not evidence of first-registration labels.
- Two to three workers per tenant: the updated orchestrator completed without a
  resume. All 144 network checks passed: 78 secondary and 66 DPU-primary.
- Both tenant clusters now have one control-plane node and three workers,
  all Ready. Both cluster workflows and all eight node workflows completed.
- The raw Kubernetes watch captured the creation of
  `nvke-dpu-1df71542-vmaas-w-5942` with `k8s.ovn.org/dpu-host=""` and
  `nodeType=gpu`. It started from a nonempty API revision and did not replay
  pre-existing Node creation events.
- GPU-selected probe pods scheduled onto all three secondary workers and
  became Ready. The host checks verified kubelet bootstrap labels, IPv6
  settings, schedulability, preserved management addresses/routes and absence
  of host OVS bridges.
- The original host/DPU pairings and addresses were preserved during scaling.

The traffic matrix covers same-node and every cross-node pod pair,
pod/node traffic in both directions, management and gateway-VF paths,
node-to-Service access, DNS and authenticated Kubernetes API Service access.
Additional checks passed for all four BGP peers, RouteAdvertisements,
external traffic and expected SNAT addresses. Fabric capture on the original
pod pair observed both TCP directions without encapsulation.

Primary and offload OVN used separate network namespaces, OVS databases and
representors on all four DPU fixtures. IPv6 sockets bound to `::1` in both
namespaces. The DPU cluster retained ordinary OVN as its primary CNI.

## Orchestrator fixes and local checks

Preparation uses the standard NKE Make targets. Image builds and simulator
startup with SQLite passed. Full controller and agent tests passed; both
module linters reported zero issues.

The orchestrator now validates the provider chart before allocation, waits for
node image preload, keeps host/DPU pairings stable during scaling, pins the
lab webhook to its advertised control-plane host, and captures Node creation
through the raw Kubernetes API with a real list revision. The traffic checks
now cover every node instead of assuming two nodes per cluster.

## Evidence and remaining issue scope

Private evidence is under `.state/managed-1df71542/`:
`before-scale-2/`, `before-scale-3/`, `network-matrix.json`,
`management-validation.json`, `host-provisioning-validation.json`,
`worker-registration-events-3.json`, `worker-registration-validation.json`,
`dpu-primary-validation.json`, and `resource-preservation-validation.json`.
The command transcripts are `.state/final-fresh-create.log`,
`.state/final-scale-resume.log` and `.state/final-scale-three.log`.

This validates the CNI selection, host OVS skip, kubelet labels and software
networking portions of [issue #1666](https://github.com/dsx-ai-factory/nvke/issues/1666).
It does not complete that issue. Production OS IPv6 boot checks, persistent
hardware VF/VFIO and uplink/Netplan preparation, and DPU-specific checks before
ReadyToJoin remain to be implemented and qualified. Whole-node reboot and
replacement have not been tested.

The lab uses veth VFs, a patched NVIDIA simulation image, fixture chart
packaging and emulated cloud-config execution. It does not qualify physical
DPUs, the Packer OS image, DPF, full VMaaS/KubeVirt deployment, SPIFFE/OIDC or
automatic credential rotation. No production POC resources were changed.

Commands and pinned inputs are in [README.md](README.md).
