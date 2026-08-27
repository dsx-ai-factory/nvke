//go:build kubeadm_bringup

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package nicovms launches the nico-vms HTTP server as a subprocess and
// exposes a thin client for the Forge API that powers the kubeadm cluster
// bring-up test.
//
// Seam: this package is the ONLY place that talks to the nico-vms binary.
// It imports the public ncx-infra-controller-rest SDK — the same wire API
// nico-vms serves — and never imports nico-vms internal packages. The
// "forge" naming throughout is deliberate: nico-vms *emulates* the
// Forge/Carbide API, so these names identify the upstream API, not the tool.
//
// Invocation contract: nico-vms parses NO command-line flags. cmd/nico-vms
// switches on os.Args[1] for "test-server" only, with no default case, so
// any flag passed here would be silently ignored. Every setting below is
// therefore an NICO_VMS_* environment variable.
package nicovms

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	forgesdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"
)

// Per-HTTP-request timeout on Forge SDK Execute() calls. Forge HTTP API
// responses normally arrive in well under a second; 15s gives generous
// headroom for a runner under load while still surfacing a hung server
// as a fast error rather than an indefinite hang on the outer ctx.
const forgeRequestTimeout = 15 * time.Second

// Seeded by nico-vms's internal/seed/default-seed.yaml. Hardcoded here
// because they are stable IDs the test driver references — if that seed
// changes them the test fails fast on create rather than misbehaving later.
const (
	DefaultTenantID = "d282929f-49d5-4dec-8d27-21ceb30ade19"

	// cpu2-mem4gb-disk30gb. The GitLab original used the 4 vCPU / 6 GiB /
	// 60 GiB type, which is right on native KVM. CI here has no /dev/kvm and
	// runs under TCG, where every guest vCPU is an emulated thread on the
	// host: doubling vCPUs roughly doubles the emulation cost rather than
	// halving the work. 2 vCPU / 4 GiB clears kubeadm's 2 CPU / 1.7 GiB
	// minimum, and the smaller disk keeps two qcow2 overlays comfortably
	// inside the runner's scratch space.
	DefaultInstanceTypeID = "a1b2c3d4-0005-4000-8000-000000000002"

	DefaultVpcID    = "a1b2c3d4-0002-4000-8000-000000000001"
	DefaultSubnetID = "a1b2c3d4-0003-4000-8000-000000000001"

	// nico-vms authenticates on bearer-token PRESENCE only — internal/http/
	// middleware.go checks that an Authorization: Bearer header exists and
	// never validates the value. Any non-empty string works; this one just
	// matches the CLI's default so logs line up.
	DefaultBearerToken = "nico-vmsctl-token"
	DefaultOrgID       = "novxwn552ssc"
)

// LaunchOpts configures the nico-vms subprocess.
type LaunchOpts struct {
	// BinaryPath is the absolute path to the nico-vms binary, produced by
	// `make -C controller/nico-vms build-privileged`. Use build-privileged,
	// not build: bridge mode calls `ip link add` and needs cap_net_admin.
	BinaryPath string

	// HTTPAddr is the listen address, exported as NICO_VMS_LISTEN_ADDR
	// (e.g. ":18080"). The corresponding BaseURL is "http://127.0.0.1<HTTPAddr>".
	HTTPAddr string

	// DBPath is the SQLite db path, exported as NICO_VMS_DB_PATH. Should be
	// under the test work dir so cleanup removes it. nico-vms uses the cgo
	// sqlite driver, so the binary must be built with CGO_ENABLED=1 — with
	// CGO_ENABLED=0 it links fine and then fails on the first query.
	DBPath string

	// SeedPath is the ABSOLUTE path to nico-vms's default-seed.yaml, exported
	// as NICO_VMS_SEED_PATH. This must be set: nico-vms defaults it to the
	// relative "./internal/seed/default-seed.yaml", so the process exits 1
	// when started from any directory other than the nico-vms module root.
	// Setting it absolutely is what lets us run the binary from anywhere
	// instead of chdir-ing the subprocess.
	SeedPath string

	// BridgeName and BridgeCIDR select native-bridge networking, exported as
	// NICO_VMS_BRIDGE_NAME / NICO_VMS_BRIDGE_CIDR. Leave NICO_VMS_LIBVIRT_NETWORK
	// unset (we never set it) so nico-vms uses its own bridge rather than an
	// existing libvirt network: config.UseLibvirtNetwork() is simply
	// `LibvirtNetwork != ""`. These must agree with whatever
	// scripts/setup-ci-host.sh created. Empty values fall back to nico-vms's
	// own defaults (cwebr0, and 172.30.0.0/24 on Linux).
	BridgeName string
	BridgeCIDR string

	// VMVCPUs / VMMemoryMiB / VMDiskGiB export NICO_VMS_VM_VCPUS /
	// NICO_VMS_VM_MEMORY_MIB / NICO_VMS_VM_DISK_GIB. Note these are only a
	// floor: the seeded instance type referenced on create also carries
	// cpu/memory/disk, so keep DefaultInstanceTypeID and these consistent.
	VMVCPUs     int
	VMMemoryMiB int
	VMDiskGiB   int

	// IpxeImageURL, when non-empty, switches VM creation to nico-vms's
	// iPXE provisioning path: each instance is created with an iPXE script
	// (built from the NKE node-images x86 LVM template) pointing at this
	// qcow2 URL. That path is the only one that boots under UEFI (required
	// by the UEFI + LUKS NKE node images) and injects the first-boot LUKS
	// key. When empty, VMs boot the default base image under BIOS (fine for
	// the plain Ubuntu cloud image).
	IpxeImageURL string

	// Stdout/Stderr where nico-vms's own logs go (typically a file under
	// the test work dir for post-mortem).
	Stdout, Stderr *os.File
}

// Process wraps a running nico-vms subprocess.
type Process struct {
	cmd          *exec.Cmd
	baseURL      string
	api          *forgesdk.APIClient
	ipxeImageURL string // when set, CreateNode provisions via an iPXE script (UEFI)
}

// Launch starts nico-vms and waits until its HTTP API answers
// authenticated requests. Returns a Process whose Stop() must be called
// (typically via defer) regardless of success/failure of downstream work.
func Launch(ctx context.Context, opts LaunchOpts) (*Process, error) {
	if opts.BinaryPath == "" {
		return nil, fmt.Errorf("nicovms.Launch: BinaryPath required")
	}
	if opts.HTTPAddr == "" {
		opts.HTTPAddr = ":18080"
	}
	if opts.DBPath == "" {
		return nil, fmt.Errorf("nicovms.Launch: DBPath required")
	}
	if opts.SeedPath == "" {
		return nil, fmt.Errorf("nicovms.Launch: SeedPath required (nico-vms defaults it relative to its own module root and exits 1 when it cannot be read)")
	}
	if !filepath.IsAbs(opts.SeedPath) {
		return nil, fmt.Errorf("nicovms.Launch: SeedPath %q must be absolute", opts.SeedPath)
	}
	if opts.VMVCPUs == 0 {
		opts.VMVCPUs = 2
	}
	if opts.VMMemoryMiB == 0 {
		opts.VMMemoryMiB = 4096
	}
	if opts.VMDiskGiB == 0 {
		opts.VMDiskGiB = 30
	}

	// No arguments: nico-vms parses no flags (see the package doc). Passing
	// --http-addr/--db-path here would be silently ignored and the server
	// would quietly come up on its defaults instead.
	cmd := exec.CommandContext(ctx, opts.BinaryPath)
	env := append(os.Environ(),
		"NICO_VMS_LISTEN_ADDR="+opts.HTTPAddr,
		"NICO_VMS_DB_PATH="+opts.DBPath,
		"NICO_VMS_SEED_PATH="+opts.SeedPath,
		"NICO_VMS_VM_VCPUS="+strconv.Itoa(opts.VMVCPUs),
		"NICO_VMS_VM_MEMORY_MIB="+strconv.Itoa(opts.VMMemoryMiB),
		"NICO_VMS_VM_DISK_GIB="+strconv.Itoa(opts.VMDiskGiB),
		// A taken :2222 is fatal at startup rather than a warning, and the
		// console SSH listener is of no use to this test. -1 skips it.
		"NICO_VMS_CONSOLE_SSH_PORT=-1",
		// No BGP/kube-vip in a two-node bring-up.
		"NICO_VMS_BGP_ENABLED=false",
	)
	if opts.BridgeName != "" {
		env = append(env, "NICO_VMS_BRIDGE_NAME="+opts.BridgeName)
	}
	if opts.BridgeCIDR != "" {
		env = append(env, "NICO_VMS_BRIDGE_CIDR="+opts.BridgeCIDR)
	}
	cmd.Env = env
	cmd.Stdout = opts.Stdout
	cmd.Stderr = opts.Stderr
	// Deliberately no cmd.Dir. The original derived a working directory from
	// BinaryPath purely so nico-vms could resolve its relative seed path;
	// NICO_VMS_SEED_PATH above removes that coupling, so the binary may live
	// anywhere.
	//
	// New process group so we can kill the whole tree, not just the nico-vms
	// parent (libvirt and qemu may be children depending on the backend).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("nicovms.Launch: start %s: %w", opts.BinaryPath, err)
	}

	baseURL := "http://127.0.0.1" + opts.HTTPAddr
	if strings.HasPrefix(opts.HTTPAddr, "0.0.0.0") {
		baseURL = "http://127.0.0.1" + strings.TrimPrefix(opts.HTTPAddr, "0.0.0.0")
	}

	cfg := forgesdk.NewConfiguration()
	cfg.Servers = forgesdk.ServerConfigurations{{URL: baseURL}}
	cfg.DefaultHeader = map[string]string{"Authorization": "Bearer " + DefaultBearerToken}
	api := forgesdk.NewAPIClient(cfg)

	p := &Process{cmd: cmd, baseURL: baseURL, api: api, ipxeImageURL: opts.IpxeImageURL}

	if err := p.waitReady(ctx); err != nil {
		// Best-effort kill if the server never came up.
		_ = p.Stop()
		return nil, fmt.Errorf("nicovms.Launch: server never became ready: %w", err)
	}
	return p, nil
}

func (p *Process) waitReady(ctx context.Context) error {
	deadline := time.Now().Add(60 * time.Second)
	client := &http.Client{Timeout: 3 * time.Second}
	for {
		// We probe the authenticated list endpoint rather than /healthz
		// (which may not exist in all nico-vms versions). If it 200s
		// or even 404s with a JSON body, the HTTP server is up.
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
			p.baseURL+"/v2/org/"+DefaultOrgID+"/forge/instance", nil)
		req.Header.Set("Authorization", "Bearer "+DefaultBearerToken)
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				return nil
			}
		}
		// Surface a dead subprocess immediately instead of timing out.
		if p.cmd.ProcessState != nil {
			return fmt.Errorf("nico-vms exited early (state=%s) before HTTP came up", p.cmd.ProcessState)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for nico-vms HTTP at %s", p.baseURL)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// BaseURL returns the http://host:port the nico-vms API listens on.
func (p *Process) BaseURL() string { return p.baseURL }

// API returns the ncx-infra-controller-rest SDK client wired to talk to this nico-vms.
func (p *Process) API() *forgesdk.APIClient { return p.api }

// Stop signals the nico-vms process group to terminate and waits for it.
// Safe to call multiple times.
func (p *Process) Stop() error {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	if p.cmd.ProcessState != nil {
		return nil // already exited
	}
	// Negative PID → process group (matches Setpgid above).
	_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- p.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}
	return nil
}

// buildIpxeScript returns an iPXE script for booting an NKE x86_64 LVM node
// image, mirroring nke/node-images/templates/x86-lvm-ipxe-script.txt. nico-vms
// parses image_url, luks.key, console/kernel params, and update_grub_* out of
// it; because the instance now carries an iPXE script, nico-vms boots the
// downloaded qcow2 under UEFI (required by the UEFI + LUKS image) and injects
// the first-boot LUKS key (luks.key=changeit; the node's TPM re-binding removes
// it after first boot).
//
// The boot-loader URL is required by the template grammar but is never fetched
// by nico-vms — it boots the qcow2 directly instead of chainloading
// qcow-imager.efi. cloud-init still comes from nico-vms (seed.iso / its EC2
// IMDS at 169.254.169.254:7777), so the cloud-init-ds line is inert here.
func buildIpxeScript(imageURL string) string {
	return `#!ipxe
set boot-loader http://127.0.0.1/qcow-imager.efi
set image-url ` + imageURL + `
set disk-strategy smallest
set console-config console=tty0 console=ttyS1,115200
set kernel-params pci=realloc=off reboot=pci loglevel=1 iommu=pt intel_iommu=on
set luks-key luks.key=changeit
set cloud-init-ds ds=nocloud-net;s=${cloudinit-url}
chain ${boot-loader} ${console-config} ${kernel-params} ${luks-key} image_url=${image-url} disk_strategy=${disk-strategy} ${cloud-init-ds} update_grub_template=yes update_grub_cfg=yes
boot
`
}

// CreateNode POSTs a single VM creation request, polls until the instance
// reaches observed state "ready" with an assigned IP, and returns its
// instance ID and primary IP.
//
// userData is the rendered cloud-init body (typically from
// internal/cloudinit.Render). phoneHomeEnabled=true makes "ready" mean
// cloud-init's runcmd has fully completed.
func (p *Process) CreateNode(ctx context.Context, name, userData string, phoneHomeEnabled bool) (string, string, error) {
	req := &forgesdk.InstanceCreateRequest{
		Name:           name,
		TenantId:       DefaultTenantID,
		InstanceTypeId: forgesdk.PtrString(DefaultInstanceTypeID),
		VpcId:          DefaultVpcID,
		Labels:         map[string]string{"nke.nvidia.com/test-role": "cluster-bringup"},
		Interfaces: []forgesdk.InterfaceCreateRequest{
			{SubnetId: forgesdk.PtrString(DefaultSubnetID)},
		},
		UserData:         *forgesdk.NewNullableString(forgesdk.PtrString(userData)),
		PhoneHomeEnabled: forgesdk.PtrBool(phoneHomeEnabled),
	}

	// NKE node images are UEFI + LUKS-encrypted. nico-vms only boots the
	// default base image under BIOS, which can't boot them. Setting an iPXE
	// script routes creation through nico-vms's iPXE path, which boots the
	// downloaded qcow2 under UEFI and injects the first-boot LUKS key. The
	// plain Ubuntu cloud-image path (no URL) stays on BIOS.
	if p.ipxeImageURL != "" {
		req.IpxeScript = *forgesdk.NewNullableString(forgesdk.PtrString(buildIpxeScript(p.ipxeImageURL)))
	}

	createCtx, createCancel := context.WithTimeout(ctx, forgeRequestTimeout)
	defer createCancel()
	created, _, err := p.api.InstanceAPI.CreateInstance(createCtx, DefaultOrgID).
		InstanceCreateRequest(*req).Execute()
	if err != nil {
		return "", "", fmt.Errorf("nicovms.CreateNode: create %s: %w", name, err)
	}
	id := created.GetId()
	if id == "" {
		return "", "", fmt.Errorf("nicovms.CreateNode: empty id in create response for %s", name)
	}

	// Poll until ready + IP assigned. 20 min budget covers cloud-init
	// apt-install of nke-kubeadm-distro (~3-5 min on cold image cache).
	deadline := time.Now().Add(20 * time.Minute)
	for {
		// Per-iteration RPC timeout — don't defer across iterations.
		pollCtx, pollCancel := context.WithTimeout(ctx, forgeRequestTimeout)
		got, _, err := p.api.InstanceAPI.GetInstance(pollCtx, DefaultOrgID, id).Execute()
		pollCancel()
		if err == nil {
			state := got.GetStatus()
			ifaces := got.GetInterfaces()
			if state == "Ready" && len(ifaces) > 0 && len(ifaces[0].GetIpAddresses()) > 0 {
				return id, ifaces[0].GetIpAddresses()[0], nil
			}
			if state == "Error" {
				return id, "", fmt.Errorf("nicovms.CreateNode: instance %s reached %q", id, state)
			}
		}
		if time.Now().After(deadline) {
			return id, "", fmt.Errorf("nicovms.CreateNode: instance %s never reached ready within budget", id)
		}
		select {
		case <-ctx.Done():
			return id, "", ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// DeleteNode deletes an instance and polls until the API confirms removal.
// Best-effort — used in cleanup paths; logs but does not return errors so
// teardown of N nodes can proceed past a single bad delete.
func (p *Process) DeleteNode(ctx context.Context, id string) error {
	if id == "" {
		return nil
	}
	delCtx, delCancel := context.WithTimeout(ctx, forgeRequestTimeout)
	_, err := p.api.InstanceAPI.DeleteInstance(delCtx, DefaultOrgID, id).Execute()
	delCancel()
	if err != nil {
		return fmt.Errorf("nicovms.DeleteNode: delete %s: %w", id, err)
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		// Per-iteration RPC timeout — don't defer across iterations.
		pollCtx, pollCancel := context.WithTimeout(ctx, forgeRequestTimeout)
		_, resp, err := p.api.InstanceAPI.GetInstance(pollCtx, DefaultOrgID, id).Execute()
		pollCancel()
		if err != nil && resp != nil && resp.StatusCode == http.StatusNotFound {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("nicovms.DeleteNode: %s did not 404 within budget", id)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}
