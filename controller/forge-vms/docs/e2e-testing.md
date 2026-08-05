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
| `FORGE_VMS_TEST_PRODUCTION_IMAGE` | unset | Set to `true` to enable the production image test |

### Debugging

| Variable | Default | Description |
|----------|---------|-------------|
| `FORGE_VMS_E2E_TESTS_KEEP_VMS` | unset | Set to `true` to keep VMs running after tests pass. Logs the instance ID and IP so you can SSH in and inspect. You must delete VMs manually when done. |
| `FORGE_VMS_PRESERVE_LOGS` | unset | Set to `true` to copy console logs and cloud-init artifacts to `/tmp/forge-vms-preserved/{test}/{instanceID}/` before VMs are deleted. |

### Timing

| Variable | Default | Description |
|----------|---------|-------------|
| `FORGE_VMS_FLOW_TIMEOUT` | 2min (real VM), 3s (mock) | How long to wait for an instance to reach "ready" |
| `FORGE_VMS_FLOW_POLL_INTERVAL` | 2s (real VM), 50ms (mock) | How often to poll instance status |
| `FORGE_VMS_TEST_PROD_IMAGE_TIMEOUT` | 15min | Readiness timeout for production image test |
| `FORGE_VMS_READINESS_TIMEOUT` | 5min | Engine-level readiness timeout before marking instance as errored |
| `FORGE_VMS_READINESS_PROBE_INTERVAL` | 3s | How often the engine probes SSH |

### VM resources

| Variable | Default | Description |
|----------|---------|-------------|
| `FORGE_VMS_VM_VCPUS` | 2 | vCPUs per VM |
| `FORGE_VMS_VM_MEMORY_MIB` | 2048 | Memory per VM in MiB |
| `FORGE_VMS_VM_DISK_GIB` | 20 | Disk size per VM in GiB |

### Infrastructure

| Variable | Default | Description |
|----------|---------|-------------|
| `FORGE_VMS_BRIDGE_NAME` | cwebr0 | Bridge interface name |
| `FORGE_VMS_BRIDGE_CIDR` | 172.30.0.0/24 | Bridge subnet |
| `FORGE_VMS_DHCP_HOSTS_DIR` | /var/lib/forge-vms/dhcp-hosts | dnsmasq DHCP host files directory |
| `FORGE_VMS_DHCP_LEASE_FILE` | /var/run/forge-vms-dnsmasq.leases | dnsmasq lease file path |
| `FORGE_VMS_IMAGE_CACHE_DIR` | ~/.cache/forge-vms/images | Cached qcow2 images |
| `FORGE_VMS_BASE_IMAGE` | auto-downloaded | Path to Ubuntu cloud image for non-iPXE tests |

## Running on a remote Linux machine

The production image test requires KVM or TCG on Linux. If you're developing on macOS, sync code to a remote machine and run there.

For the full CWE + forge-vms Tilt day-2 loop (Mutagen sync of multiple repos, UI
and kubectl forwards from a Colossus host), see
[Remote Colossus Tilt](../../docs/remote-colossus-tilt.md). The steps below are
the lighter one-shot path for forge-vms integration tests only.

### One-time setup

SSH into the remote and run the setup script as root:

```bash
ssh my-machine.nvidia.com
sudo bash ~/dev/cloud-workflow-engine/forge-vms/scripts/setup-linux.sh
```

This configures: bridge networking, dnsmasq DHCP, metadata IP (169.254.169.254), KVM/libvirt, qemu-bridge-helper, NAT for internet access.

### Sync code

```bash
rsync -avz \
  --exclude='.git' --exclude='vendor' --exclude='.cache' \
  --exclude='.gocache' --exclude='.gomodcache' \
  --exclude='var' --exclude='images' --exclude='bin' \
  forge-vms/ my-machine.nvidia.com:~/dev/cloud-workflow-engine/forge-vms/
```

### Run tests

The remote shell is csh/tcsh — always wrap commands in `bash -lc`:

```bash
# Production image test
ssh my-machine.nvidia.com 'bash -lc "cd ~/dev/cloud-workflow-engine/forge-vms && \
  FORGE_VMS_TEST_PRODUCTION_IMAGE=true \
  FORGE_VMS_DHCP_HOSTS_DIR=/var/lib/forge-vms/dhcp-hosts \
  FORGE_VMS_DHCP_LEASE_FILE=/var/run/forge-vms-dnsmasq.leases \
  go test -tags=integration -count=1 -timeout 20m -v \
  ./test/integration/libvirt -run TestIPXEProductionImageFlow"'
```

### Debug a running VM

Keep the VM alive after the test:

```bash
ssh my-machine.nvidia.com 'bash -lc "cd ~/dev/cloud-workflow-engine/forge-vms && \
  FORGE_VMS_TEST_PRODUCTION_IMAGE=true \
  FORGE_VMS_E2E_TESTS_KEEP_VMS=true \
  FORGE_VMS_PRESERVE_LOGS=true \
  FORGE_VMS_DHCP_HOSTS_DIR=/var/lib/forge-vms/dhcp-hosts \
  FORGE_VMS_DHCP_LEASE_FILE=/var/run/forge-vms-dnsmasq.leases \
  go test -tags=integration -count=1 -timeout 20m -v \
  ./test/integration/libvirt -run TestIPXEProductionImageFlow"'
```

The test output will show the IP. SSH into the VM:

```bash
ssh my-machine.nvidia.com   # get on the remote
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

If a test times out or you used `FORGE_VMS_E2E_TESTS_KEEP_VMS`, clean up manually:

```bash
ssh my-machine.nvidia.com 'bash -lc "
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
cat /tmp/forge-vms-integration/libvirt-*/instances/*/disk.qcow2.ttyS1.log | strings | less

# Or if FORGE_VMS_PRESERVE_LOGS was set
cat /tmp/forge-vms-preserved/*/*/disk.qcow2.ttyS1.log | strings | less
```

- `ttyS0.log` — output from cloud images (COM1)
- `ttyS1.log` — output from production bare-metal images (COM2)
