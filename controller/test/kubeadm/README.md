# kubeadm cluster bring-up on nico-vms

An advisory end-to-end test that boots two Ubuntu 24.04 VMs through
[nico-vms](../../nico-sim/README.md), then drives `nke-agent`'s gRPC kubeadm
RPCs to run `kubeadm init` on the first VM and join the second to it.

It is a port of the kubeadm builder repository's `cluster-bringup` driver into
the `controller` module, cut down to the smallest shape that still proves
cross-VM cluster formation: two VMs instead of four, one worker join instead of
two control-plane joins plus a worker, and no CNI.

The whole suite sits behind the `kubeadm_bringup` build tag, so nothing here
compiles or runs unless you ask for it by name.

## What this proves, and what it does not

**Proves.** nico-vms creates two VMs on the host; cloud-init installs an upstream
Kubernetes stack; **nke-agent** — built from the working tree, not a published
package — drives `kubeadm init` and one worker join over gRPC; upstream Calico
installs; every node reports `Ready`. That is functional coverage of nke-agent's
kubeadm RPCs, which nothing else in this repo has.

**Does not prove.** That NKE's own packages work. The Kubernetes stack comes from
`dl.k8s.io` and GitHub releases, and the CNI from upstream Calico's manifests —
not from `nke-kubeadm-distro` or `nke-cni-*`. Those live behind a source-IP
allowlist that CI runners cannot reach. Verifying the shipped artifacts stays
with the GitLab `cluster-bringup` suite, which can reach them and runs the fuller
matrix (4 VMs, HA etcd, OVN or Calico).

The distinction matters: a green run here means *the cluster nke-agent builds is
usable*, not *NKE's packaging is good*.

**Not the production CNI path either.** The agent's `InstallCNI` RPC requires an
NKE-named Debian package registered with dpkg, so this suite applies Calico's
manifests directly rather than impersonating one.

## Layout

| Path | Purpose |
|---|---|
| `internal/nicovms/` | Launches the `nico-vms` binary as a subprocess and wraps the Forge SDK client (`CreateNode`/`CreateNodes`/`DeleteNode` with ready-state polling). The only place that talks to nico-vms; no nico-vms internal package is imported. |
| `internal/cluster/` | gRPC client for `nke-agent`: `ExecuteInit`, `ExecuteJoin`, `GetJoinStrings`, `WaitForPhase`, `WaitForAgent`, `FetchKubeconfig`, and `RunScript` for on-failure diagnostics. |
| `internal/cloudinit/` | Renders `testdata/user-data.yml.tmpl` by substituting the `__KEY__` placeholders. Every placeholder must resolve or `Render` fails. |
| `testdata/user-data.yml.tmpl` | Per-VM cloud-init. The install block is a `__K8S_PROVISION__` placeholder filled by `internal/cloudinit`; it also drops the `--disable-mtls` systemd override. |
| `scripts/setup-ci-host.sh` | Prepares a CI runner for native bridge mode: QEMU/libvirt packages, the bridge, dnsmasq, `ip_forward`, NAT and FORWARD rules. A trimmed lift of nico-vms' `setup-linux.sh` networking half. |
| `scripts/teardown-ci-host.sh` | Undoes the above, best-effort, and copies the VM console logs into the artifact directory. |

## The build tag

Every file in this tree begins with `//go:build kubeadm_bringup`. Without the
tag the packages do not exist as far as the toolchain is concerned:

```console
$ go test -C controller ./test/kubeadm/...
go: warning: "./test/kubeadm/..." matched no packages
no packages to test
```

The tag exists because this suite needs real VMs, a host bridge, and outbound
network access to a public Kubernetes mirror. A plain `go test ./...` anywhere in
the `controller` module must never try to boot a VM. The tag also keeps the
suite inside the existing `controller` module — no new `go.mod`, no `go.work`
entry, no `vendor/` churn — while `.github/workflows/controller-test.yml`'s
`compile` matrix still type-checks it on every pull request with
`go vet -tags kubeadm_bringup`.

## Prerequisites (local Linux host)

You need a Linux host with hardware virtualisation and libvirt. A Colossus
bare-metal lease works; so does any workstation with `/dev/kvm`. macOS cannot
run this suite — nico-vms' libvirt backend is Linux-only.

1. **Host packages and networking.** nico-vms' own setup script installs QEMU,
   libvirt, swtpm and OVMF, creates the `cwebr0` bridge on `172.30.0.0/24`,
   starts a dnsmasq for DHCP, and installs the iptables NAT rules
   (`../../nico-sim/README.md:641-656`):

   ```bash
   sudo ./controller/nico-sim/scripts/setup-linux.sh
   ```

   Log out and back in afterwards — `libvirt`/`kvm` group membership does not
   apply to the current session.

2. **An Ubuntu 24.04 cloud image.** Vanilla noble, BIOS boot. This suite does
   not use the NKE FDE node image, so there is no UEFI, LUKS or swtpm
   requirement and no metadata server dependency:

   ```bash
   mkdir -p controller/nico-sim/images
   curl -fL -o controller/nico-sim/images/ubuntu-cloud.qcow2 \
     https://cloud-images.ubuntu.com/releases/24.04/release/ubuntu-24.04-server-cloudimg-amd64.img
   ```

   That is the same URL nico-vms' `test-server` mode downloads
   (`../../nico-sim/cmd/nico-sim/testserver.go:198-203`).

3. **The `nico-vms` binary, built with cgo and `CAP_NET_ADMIN`.**

   ```bash
   CGO_ENABLED=1 make -C controller/nico-sim build-privileged
   ```

   Both halves matter. nico-vms' state store imports `mattn/go-sqlite3`
   (`../../nico-sim/internal/state/store.go:16`), which is a cgo binding: with
   `CGO_ENABLED=0` the binary **compiles cleanly and dies at the first query**.
   And `build-privileged` is `build` plus `sudo setcap cap_net_admin+ep`
   (`../../nico-sim/Makefile:14,33-35`), which bridge mode needs to add
   addresses and install routes. `setup-linux.sh` installs a sudoers rule so
   the `setcap` does not prompt.

4. **Nothing from NVIDIA.** In the default `community` mode the VMs fetch
   Kubernetes from `dl.k8s.io`, containerd/runc/CNI-plugins/crictl from GitHub
   releases, and Calico from `raw.githubusercontent.com`. `nke-agent` is built
   locally and served to the guests over the bridge. No NVIDIA network needed.

```bash
# Provisioning mode. "community" (the default) fetches upstream Kubernetes and
# needs no NVIDIA network. "apt" uses the NKE apt repository and only works from
# an allowlisted host.
export NKE_BRINGUP_PROVISION_MODE=community

# Where the base image lives. Make it absolute: nico-vms resolves a relative
# path against its own working directory, which is not the one you are in.
export NICO_VMS_BASE_IMAGE="$PWD/controller/nico-sim/images/ubuntu-cloud.qcow2"

# Match what setup-linux.sh configured.
export NICO_VMS_BRIDGE_NAME=cwebr0
export NICO_VMS_BRIDGE_CIDR=172.30.0.0/24

# Do not fight another nico-vms for the console SSH port; -1 disables it.
export NICO_VMS_CONSOLE_SSH_PORT=-1

go test -C controller -tags kubeadm_bringup -timeout 90m -v ./test/kubeadm/...
```

`-timeout` is not optional. Go's default is 10 minutes and a two-VM bring-up
does not finish in that, even with KVM.

### Configuration

Test-driver knobs live in the `NKE_BRINGUP_*` namespace. `NICO_VMS_*` is
nico-vms' own namespace and is never used for driver settings.

| Variable | Default | Purpose |
|---|---|---|
| `NKE_BRINGUP_KUBEADM_DEB` | unset | Only for `NKE_BRINGUP_PROVISION_MODE=apt`; e.g. `nke-kubeadm-distro-1.36.3` |
| `NKE_BRINGUP_CALICO_VERSION` | `v3.31.4` | Upstream Calico stream to install |
| `NKE_BRINGUP_CNI_TIMEOUT` | `20m` | Budget for the Calico install and its CRD waits |
| `NKE_BRINGUP_READY_TIMEOUT` | `30m` | Budget for all nodes to report `Ready` |
| `NKE_BRINGUP_KUBEADM_DEB_VERSION` | the suite's pin | Exact apt-pinned version, e.g. `5` |
| `NKE_BRINGUP_INIT_TIMEOUT` | `15m` | Wall clock for `ExecuteKubeadmInit` → `phase=initialize` |
| `NKE_BRINGUP_JOIN_TIMEOUT` | `10m` | Wall clock for `ExecuteKubeadmJoin` → `phase=join` |
| `NKE_BRINGUP_ARTIFACT_DIR` | *(unset)* | Where the suite, the teardown script and CI's upload step meet: rendered cloud-init, the nico-sim log, and the VM console logs. |

The CI lane sets neither package variable, so it exercises whatever the suite
pins. Set both when you are validating a specific apt publish — that is what
this test is for.

The apt repository coordinates are compiled-in defaults and normally need no
override: suite `nke-noble` (**not** plain `noble`), component `main`, served
from the public upstream mirrors.

`.github/workflows/kubeadm-bringup-e2e.yml` is the canonical worked example of
the whole environment; it explains each value in place.

These nico-vms variables are read by the server the driver launches. The driver
sets `NICO_SIM_LISTEN_ADDR`, `NICO_SIM_DB_PATH` and `NICO_SIM_SEED_PATH`
itself — nico-vms parses **zero** command-line flags, so everything is
environment (`../../nico-sim/internal/config/config.go:157-181`). The rest are
yours to set:

| Variable | Default | Notes |
|---|---|---|
| `NICO_VMS_BASE_IMAGE` | `images/ubuntu-cloud.qcow2` | Required for any non-mock backend (`config.go:165,229`). Relative paths resolve against nico-vms' working directory. |
| `NICO_VMS_BRIDGE_NAME` | `cwebr0` | `config.go:163` |
| `NICO_VMS_BRIDGE_CIDR` | `172.30.0.0/24` | Unset means the default is logged and applied at normalisation (`config.go:164,419-422`). |
| `NICO_VMS_CONSOLE_SSH_PORT` | `2222` | `-1` disables. A port already in use is fatal (`config.go:128`). |
| `NICO_VMS_READINESS_TIMEOUT` | `15m` | Per-VM boot budget. Raise it under emulation (`config.go:101`). |
| `NICO_VMS_VM_VCPUS` / `_VM_MEMORY_MIB` / `_VM_DISK_GIB` | `2` / `2048` / `20` | The driver overrides these with kubeadm-workable values (`config.go:111-119`). |
| `NICO_VMS_LIBVIRT_NETWORK` | *(unset)* | **Leave unset.** Setting it switches nico-vms to libvirt-network mode, which needs a pre-existing libvirt network and `qemu:///system`. This suite uses native bridge mode. |

## KVM versus TCG

nico-vms picks its accelerator by stat-ing the device and says nothing about
it: `os.Stat("/dev/kvm")` returns `kvm` if present and `qemu` (TCG software
emulation) otherwise (`../../nico-sim/internal/vm/libvirt/virsh.go:248-253`).
The fallback is automatic and silent.

Locally that is fine — a workstation or Colossus lease has `/dev/kvm` and you
get hardware acceleration.

**In CI you do not.** No runner this repository can reach exposes `/dev/kvm`.
The self-hosted pool is EC2 Nitro instances rather than bare metal, so the host
exposes no `vmx`/`svm` to virtualise with, and GitHub-hosted standard runners
are Azure VMs without nested virtualisation
(`../../../.github/workflows/os-image-artifact.yml:125-127`). So the lane runs
under TCG, which costs roughly 10x
(`../../../.github/workflows/os-image-artifact.yml:133`).

That is why the workflow prints which accelerator it got before doing anything
else — the same "Report the accelerator" step `os-image-artifact.yml` uses — and
why its timeouts are set in hours rather than minutes. Slow, not prohibitive.

## Troubleshooting

**Where the logs are.** nico-vms' own log is written to the suite's work
directory. Per-VM serial console output lands next to each VM's disk overlay,
under `{db_dir}/instances/{instance_id}/`
(`../../nico-sim/README.md:587-592`):

- `disk.qcow2.ttyS0.log` — cloud images, serial port 0. This is the one you
  want.
- `disk.qcow2.ttyS1.log` — production FDE images, serial port 1. Unused here.

The `nico-simctl` helper prints them without needing the API, but it only
searches conventional locations by default, so point it at the work directory:

```bash
./controller/nico-sim/bin/nico-simctl instance console-log-local \
  --dir <work-dir>/instances --name vm-1 --dump
```

**Which accelerator did I get?** The workflow prints it, but you can check the
host directly, and confirm what libvirt actually built:

```bash
[[ -c /dev/kvm ]] && echo kvm || echo tcg

# Domains are named nico-vm-<instance-id>, not nico-vm-<vm-name>.
virsh -c qemu:///session list --all --name | grep '^nico-vm-'
virsh -c qemu:///session dumpxml <domain> | grep '<domain type='
```

`<domain type='kvm'>` is hardware-accelerated; `<domain type='qemu'>` is TCG.
Under TCG, raise `NICO_VMS_READINESS_TIMEOUT`, `NKE_BRINGUP_INIT_TIMEOUT`,
`NKE_BRINGUP_JOIN_TIMEOUT` and `go test -timeout` together — a budget that fits
locally will not fit emulated.

**The AWS metadata address is deliberately missing in CI.**
`setup-linux.sh` adds `169.254.169.254/32` to the bridge so production images
can reach nico-vms' EC2-style metadata server (`setup-linux.sh:399-401`). On an
AWS runner that address **is** the EC2 instance metadata service, and adding it
installs a local route that hijacks the runner's own IMDS — credential refresh
and the runner agent's housekeeping start failing in ways that look nothing
like a VM problem. `scripts/setup-ci-host.sh` omits that one line.
Nothing here needs it: vanilla noble takes its user data from a local NoCloud
seed ISO, and nico-vms warns per address and carries on.

**`'<node-ip>' does not match any of DNSNames`.** If VM-to-VM traffic gets its
source address rewritten, IP-bound peer TLS breaks and the join fails with that
message. The usual cause is a SNAT rule that sits earlier in the nat
`POSTROUTING` chain than nico-vms' own MASQUERADE — most notably the AWS VPC
CNI's `AWS-SNAT-CHAIN`, which masquerades anything not bound for the VPC CIDR
(`../../nico-sim/README.md:167`). `setup-linux.sh` appends its rule, so it
loses the race. Exempt intra-subnet traffic by *inserting* a `RETURN` at
position 1, the same fix `setup-libvirt-network.sh` applies:

```bash
sudo iptables -t nat -I POSTROUTING 1 \
  -s 172.30.0.0/24 -d 172.30.0.0/24 \
  -m comment --comment nico-vms-ci -j RETURN
```

**`dial tcp <ip>:50051: connect: connection refused`.** The VM booted but
`nke-agent` is not serving yet — cloud-init is still running its `apt-get
install`. The driver polls for this (`WaitForAgent`), so a persistent refusal
means cloud-init failed. Read the VM's console log, then its
`/var/log/cloud-init-output.log`.

**`qemu:///session: connection refused`.** Your user is not in the `libvirt`
group yet. Log out and back in after `setup-linux.sh`.

**Stale VMs after a killed run.** The suite deletes its instances on success
and failure, but a `SIGKILL`ed `go test` leaves domains behind:

```bash
virsh -c qemu:///session list --all --name | grep '^nico-vm-' \
  | xargs -r -n1 virsh -c qemu:///session destroy
```

## CI

This lane is **advisory**. It runs from `.github/workflows/kubeadm-bringup-e2e.yml`
on `workflow_dispatch` and on pushes to the `pull-request/<N>` mirror branches
copy-pr-bot creates, and it is deliberately **not** a member of
`merge-gate.yml`.

Two reasons, both of them repository policy rather than preference. A required
gate member must pass on a GitHub-hosted runner with fork-pull-request
permissions and no repository secrets; anything needing NVIDIA self-hosted
runners or privileged infrastructure stays advisory
(`../../../.github/workflows/README.md:25-28`). And self-hosted runners never
receive `pull_request` events at all, so the vetted mirror-branch push is both
the trigger and the security boundary
(`../../../.github/workflows/README.md:41`).

Cost is also still unmeasured. `nico-e2e.yml:16-19` records what happens when a
lane is gated on an estimate instead of a measurement: measure first, gate
later.
