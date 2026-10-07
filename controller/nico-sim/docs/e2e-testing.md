# E2E Testing Guide

## Quick start

```bash
# Mock tests (fast, no real VMs, runs anywhere)
make test-integration-mock

# Real VM tests — macOS (darwin-qemu)
make test-integration-qemu

# Real VM tests — Linux (libvirt)
make test-integration-libvirt

# Run a specific test
make test-integration-libvirt RUN_TESTS=TestIPXEProductionImageFlow
```

## Test backends

| Backend | OS | Speed | What it tests |
|---------|-----|-------|--------------|
| `mock` | Any | <1s | API, state machine, iPXE parsing, artifact creation |
| `darwin-qemu` | macOS | 20-70s | Real VM boot with Ubuntu cloud image |
| `libvirt` | Linux | 60-120s | Real VM boot with Ubuntu cloud image |
| `libvirt` + prod image | Linux | 8-15min | Full production FDE image boot (multi-GB, LUKS, TCG) |

## Test flows

| Flow | What it does |
|------|-------------|
| `TestCreatePollReadyAndDeleteFlow` | Basic lifecycle: create, wait for ready, delete |
| `TestSSHVerificationFlow` | Boot VM, SSH in, verify cloud-init marker |
| `TestPatchLabelsAndRebootFlow` | Boot, SSH, write disk marker, reboot, verify persistence |
| `TestVMToVMConnectivityFlow` | Boot two VMs, verify TCP connectivity between them |
| `TestIPXEBootFlow` | iPXE with local test image (fast) |
| `TestIPXEProductionImageFlow` | iPXE with real CloudFront production image (slow, gated) |

## Environment variables

### Test selection

| Variable | Default | Description |
|----------|---------|-------------|
| `NICO_VMS_TEST_PRODUCTION_IMAGE` | unset | Set to `true` to enable the production image test |

### Debugging

| Variable | Default | Description |
|----------|---------|-------------|
| `NICO_VMS_E2E_TESTS_KEEP_VMS` | unset | Set to `true` to keep VMs running after tests pass. Logs the instance ID and IP so you can SSH in and inspect. You must delete VMs manually when done. |
| `NICO_VMS_PRESERVE_LOGS` | unset | Set to `true` to copy console logs and cloud-init artifacts to `/tmp/nico-vms-preserved/{test}/{instanceID}/` before VMs are deleted. |

### Timing

| Variable | Default | Description |
|----------|---------|-------------|
| `NICO_VMS_FLOW_TIMEOUT` | 2min (real VM), 3s (mock) | How long to wait for an instance to reach "ready" |
| `NICO_VMS_FLOW_POLL_INTERVAL` | 2s (real VM), 50ms (mock) | How often to poll instance status |
| `NICO_VMS_TEST_PROD_IMAGE_TIMEOUT` | 15min | Readiness timeout for production image test |
| `NICO_VMS_READINESS_TIMEOUT` | 5min | Engine-level readiness timeout before marking instance as errored |
| `NICO_VMS_READINESS_PROBE_INTERVAL` | 3s | How often the engine probes SSH |

### VM resources

| Variable | Default | Description |
|----------|---------|-------------|
| `NICO_VMS_VM_VCPUS` | 2 | vCPUs per VM |
| `NICO_VMS_VM_MEMORY_MIB` | 2048 | Memory per VM in MiB |
| `NICO_VMS_VM_DISK_GIB` | 20 | Disk size per VM in GiB |

### Infrastructure

| Variable | Default | Description |
|----------|---------|-------------|
| `NICO_VMS_BRIDGE_NAME` | cwebr0 | Bridge interface name |
| `NICO_VMS_BRIDGE_CIDR` | 172.30.0.0/24 | Bridge subnet |
| `NICO_VMS_DHCP_HOSTS_DIR` | /var/lib/nico-vms/dhcp-hosts | dnsmasq DHCP host files directory |
| `NICO_VMS_DHCP_LEASE_FILE` | /var/run/nico-vms-dnsmasq.leases | dnsmasq lease file path |
| `NICO_VMS_IMAGE_CACHE_DIR` | ~/.cache/nico-sim/images | Cached qcow2 images |
| `NICO_VMS_BASE_IMAGE` | auto-downloaded | Path to Ubuntu cloud image for non-iPXE tests |

## Running on a remote Linux machine

The production image test requires KVM or TCG on Linux. If you're developing on macOS, sync code to a remote machine and run there.

The steps below are the one-shot path for nico-vms integration tests only.

### One-time setup

The setup script does not clone the repository, so clone it on the remote
first, at `~/dev/nke`. The tests need the whole repository, not only
`nico-sim/`: `TestBGPECMPFlow` reads the control-plane runbooks from
`controller/internal/runbook/scripts`. Later syncs copy only `nico-sim/` over
this clone. A `NICO_SIM_REMOTE_DIR` override must also point at the
`nico-sim/` directory of a full clone.

Then run the setup script as your own user. It calls `sudo` for the privileged
steps and writes the sudoers rule for the user who runs it:

```bash
ssh my-machine.example.com
git clone https://github.com/dsx-ai-factory/nke.git ~/dev/nke   # skip if already cloned
bash ~/dev/nke/controller/nico-sim/scripts/setup-linux.sh
```

If the remote was set up from an older path (`~/dev/cloud-workflow-engine/nico-sim`
or `~/dev/nke/cloud-workflow-engine/nico-sim`), run the setup script again from
the new path. Its passwordless `setcap` sudoers rule names the exact `nico-sim`
binary path. The sync skips `images/`, so the first test run at the new path
downloads the base image again; copy `images/` from the old directory to skip
the download.

This configures: bridge networking, dnsmasq DHCP, metadata IP (169.254.169.254), KVM/libvirt, qemu-bridge-helper, NAT for internet access.

### Sync code

```bash
rsync -avz \
  --exclude='.git' --exclude='vendor' --exclude='.cache' \
  --exclude='.gocache' --exclude='.gomodcache' \
  --exclude='var' --exclude='images' --exclude='bin' \
  nico-sim/ my-machine.example.com:~/dev/nke/controller/nico-sim/
```

### Run tests

The remote shell is csh/tcsh — always wrap commands in `bash -lc`:

```bash
# Production image test
ssh my-machine.example.com 'bash -lc "cd ~/dev/nke/controller/nico-sim && \
  NICO_VMS_TEST_PRODUCTION_IMAGE=true \
  NICO_VMS_DHCP_HOSTS_DIR=/var/lib/nico-vms/dhcp-hosts \
  NICO_VMS_DHCP_LEASE_FILE=/var/run/nico-vms-dnsmasq.leases \
  go test -tags=integration -count=1 -timeout 20m -v \
  ./test/integration/libvirt -run TestIPXEProductionImageFlow"'
```

### Debug a running VM

Keep the VM alive after the test:

```bash
ssh my-machine.example.com 'bash -lc "cd ~/dev/nke/controller/nico-sim && \
  NICO_VMS_TEST_PRODUCTION_IMAGE=true \
  NICO_VMS_E2E_TESTS_KEEP_VMS=true \
  NICO_VMS_PRESERVE_LOGS=true \
  NICO_VMS_DHCP_HOSTS_DIR=/var/lib/nico-vms/dhcp-hosts \
  NICO_VMS_DHCP_LEASE_FILE=/var/run/nico-vms-dnsmasq.leases \
  go test -tags=integration -count=1 -timeout 20m -v \
  ./test/integration/libvirt -run TestIPXEProductionImageFlow"'
```

The test output will show the IP. SSH into the VM:

```bash
ssh my-machine.example.com   # get on the remote
sshpass -p testpass ssh -o StrictHostKeyChecking=no testuser@172.30.0.XXX
```

Useful things to check inside the VM:

```bash
# Kernel command line (what GRUB passed to the kernel)
cat /proc/cmdline

# Cloud-init datasource config
cat /etc/cloud/cloud.cfg
ls -la /etc/cloud/cloud.cfg.d/
cat /etc/cloud/cloud.cfg.d/*.cfg

# Cloud-init status and datasource used
cloud-init status --long

# Cloud-init logs
less /var/log/cloud-init.log
less /var/log/cloud-init-output.log

# Network config
ip addr show
cat /etc/netplan/*.yaml
```

### Clean up orphaned VMs

If a test times out or you used `NICO_VMS_E2E_TESTS_KEEP_VMS`, clean up manually:

```bash
ssh my-machine.example.com 'bash -lc "
  for vm in \$(virsh -c qemu:///session list --all --name); do
    virsh -c qemu:///session destroy \$vm 2>/dev/null
    virsh -c qemu:///session undefine \$vm --nvram 2>/dev/null
  done
  echo Done
"'
```

### View console logs

Console output is written to files next to the disk image:

```bash
# From the remote machine
cat /tmp/nico-vms-integration/libvirt-*/instances/*/disk.qcow2.ttyS1.log | strings | less

# Or if NICO_VMS_PRESERVE_LOGS was set
cat /tmp/nico-vms-preserved/*/*/disk.qcow2.ttyS1.log | strings | less
```

- `ttyS0.log` — output from cloud images (COM1)
- `ttyS1.log` — output from production bare-metal images (COM2)
