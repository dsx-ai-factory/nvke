// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package testutil

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	forgesdk "gitlab-master.nvidia.com/nvmetal/cloud-api-sdk-go"
)

// ProductionImageURL is the default production FDE qcow2 image used by e2e tests.
const ProductionImageURL = "https://d3mq564cn7sth7.cloudfront.net/nke-isos/qcow/images/24.04/x86_64/lvm/2026-03-23/1774290565/nke-24.04-x86_64-lvm-2026-03-23-1774290565.qcow2"

// RunCreatePollReadyAndDeleteFlow creates an instance, waits for it to reach
// "ready", then deletes it and polls until the 404 confirms removal.
func RunCreatePollReadyAndDeleteFlow(t *testing.T, env *TestEnv, profile RuntimeProfile) {
	t.Helper()
	instanceName := UniqueInstanceName("node-create-delete")

	createPayload := &forgesdk.InstanceCreateRequest{
		Name:           instanceName,
		TenantId:       "d282929f-49d5-4dec-8d27-21ceb30ade19",
		InstanceTypeId: forgesdk.PtrString("itype-mock-001"),
		VpcId:          "vpc-mock-001",
		Labels:         map[string]string{"nke.nvidia.com/node-id": instanceName},
		Interfaces: []forgesdk.InterfaceCreateRequest{
			{SubnetId: forgesdk.PtrString("subnet-mock-001")},
		},
	}

	created := env.CreateInstance(createPayload)
	t.Cleanup(func() {
		PreserveInstanceArtifacts(t, profile, created.GetId())
		CleanupInstanceByID(t, env, profile, created.GetId())
	})

	readyInstance := WaitForInstanceState(t, env, profile, created.GetId(), "ready")
	ifaces := readyInstance.GetInterfaces()
	if len(ifaces) == 0 || len(ifaces[0].GetIpAddresses()) == 0 {
		t.Fatalf("[%s] instance %s reached ready without IP", profile.Name, created.GetId())
	}

	PreserveInstanceArtifacts(t, profile, created.GetId())
	ip := ifaces[0].GetIpAddresses()[0]
	DeleteAndWait(t, env, profile, created.GetId(), ip)
}

// RunPatchLabelsAndRebootFlow creates an instance with cloud-init UserData that
// enables password-based SSH and a boot counter, waits for ready, writes a unique
// disk marker via SSH, triggers a reboot, then verifies the instance returns to
// ready with:
//   - the patched label applied,
//   - the disk marker still present (proving disk persistence / same VM), and
//   - the boot counter showing exactly two boots.
//
// This flow is intended for real VM backends (libvirt, darwin-qemu).
// For mock-backend reboot tests, use a lighter test that only checks API state
// transitions (see mock_test.go).
func RunPatchLabelsAndRebootFlow(t *testing.T, env *TestEnv, profile RuntimeProfile) {
	t.Helper()
	instanceName := UniqueInstanceName("node-reboot")

	createPayload := &forgesdk.InstanceCreateRequest{
		Name:           instanceName,
		TenantId:       "d282929f-49d5-4dec-8d27-21ceb30ade19",
		InstanceTypeId: forgesdk.PtrString("itype-mock-001"),
		VpcId:          "vpc-mock-001",
		Labels:         map[string]string{"nke.nvidia.com/node-id": instanceName},
		Interfaces: []forgesdk.InterfaceCreateRequest{
			{SubnetId: forgesdk.PtrString("subnet-mock-001")},
		},
		UserData: *forgesdk.NewNullableString(forgesdk.PtrString(
			RebootFlowUserData(instanceName),
		)),
	}

	created := env.CreateInstance(createPayload)
	t.Cleanup(func() {
		PreserveInstanceArtifacts(t, profile, created.GetId())
		CleanupInstanceByID(t, env, profile, created.GetId())
	})

	readyInst := WaitForInstanceState(t, env, profile, created.GetId(), "ready")

	ip := InstanceIP(t, readyInst)
	sshCfg := TestSSHConfig()
	markerToken := fmt.Sprintf("reboot-canary-%s", created.GetId())

	t.Logf("Writing disk marker via SSH to %s", ip)
	writeCmd := fmt.Sprintf("echo %s > /var/tmp/reboot-marker", markerToken)
	WaitForSSHCommand(t, ip, sshCfg, writeCmd, 90*time.Second)

	bootCount := WaitForSSHCommand(t, ip, sshCfg, "cat /var/tmp/boot-count", 10*time.Second)
	boots := strings.Count(bootCount, "boot")
	if boots != 1 {
		t.Fatalf("[%s] expected 1 boot before reboot, got %d (content: %q)", profile.Name, boots, bootCount)
	}
	t.Logf("Pre-reboot checks passed: marker written, boot count = %d", boots)

	env.PatchInstance(created.GetId(), &forgesdk.InstanceUpdateRequest{
		Labels:        map[string]string{"nke.nvidia.com/ip-address": "192.168.200.10"},
		TriggerReboot: *forgesdk.NewNullableBool(forgesdk.PtrBool(true)),
	})

	WaitForInstanceStateChange(t, env, profile, created.GetId(), "ready")
	updated := WaitForInstanceState(t, env, profile, created.GetId(), "ready")
	if updated.GetLabels()["nke.nvidia.com/ip-address"] != "192.168.200.10" {
		t.Fatalf("[%s] instance %s reached ready without patched label", profile.Name, created.GetId())
	}

	ip = InstanceIP(t, updated)

	t.Logf("Verifying disk marker survived reboot via SSH to %s", ip)
	got := WaitForSSHCommand(t, ip, sshCfg, "cat /var/tmp/reboot-marker", 90*time.Second)
	if strings.TrimSpace(got) != markerToken {
		t.Fatalf("[%s] disk marker mismatch after reboot: got %q, want %q", profile.Name, strings.TrimSpace(got), markerToken)
	}

	bootCount = WaitForSSHCommand(t, ip, sshCfg, "cat /var/tmp/boot-count", 10*time.Second)
	boots = strings.Count(bootCount, "boot")
	if boots != 2 {
		t.Fatalf("[%s] expected 2 boots after reboot, got %d (content: %q)", profile.Name, boots, bootCount)
	}
	t.Logf("Post-reboot checks passed: marker intact, boot count = %d", boots)

	PreserveInstanceArtifacts(t, profile, created.GetId())
	DeleteAndWait(t, env, profile, created.GetId(), ip)
}

// RunVMToVMConnectivityFlow creates two instances on the same subnet, waits for
// both to reach "ready", then verifies bidirectional TCP connectivity. Each VM
// boots with a netcat listener (via cloud-init runcmd) that writes received data
// to a file. The test SSHes into each VM to send a unique token to the other,
// then reads back the received files to confirm the tokens arrived.
//
// This flow is intended for real VM backends (libvirt, darwin-qemu).
func RunVMToVMConnectivityFlow(t *testing.T, env *TestEnv, profile RuntimeProfile) {
	t.Helper()

	nameA := UniqueInstanceName("vm2vm-a")
	nameB := UniqueInstanceName("vm2vm-b")
	tokenAtoB := fmt.Sprintf("a2b-%s", nameA)
	tokenBtoA := fmt.Sprintf("b2a-%s", nameB)
	const ncPort = "9999"
	const outFile = "/tmp/vm2vm-received"

	createPayload := func(name, userData string) *forgesdk.InstanceCreateRequest {
		return &forgesdk.InstanceCreateRequest{
			Name:           name,
			TenantId:       "d282929f-49d5-4dec-8d27-21ceb30ade19",
			InstanceTypeId: forgesdk.PtrString("itype-mock-001"),
			VpcId:          "vpc-mock-001",
			Labels:         map[string]string{"nke.nvidia.com/node-id": name},
			Interfaces: []forgesdk.InterfaceCreateRequest{
				{SubnetId: forgesdk.PtrString("subnet-mock-001")},
			},
			UserData: *forgesdk.NewNullableString(forgesdk.PtrString(userData)),
		}
	}

	vmA := env.CreateInstance(createPayload(nameA, connectivityFlowUserData(nameA, ncPort, outFile)))
	t.Logf("VM A created: id=%s name=%s", vmA.GetId(), vmA.GetName())
	t.Cleanup(func() {
		PreserveInstanceArtifacts(t, profile, vmA.GetId())
		CleanupInstanceByID(t, env, profile, vmA.GetId())
	})

	vmB := env.CreateInstance(createPayload(nameB, connectivityFlowUserData(nameB, ncPort, outFile)))
	t.Logf("VM B created: id=%s name=%s", vmB.GetId(), vmB.GetName())
	t.Cleanup(func() {
		PreserveInstanceArtifacts(t, profile, vmB.GetId())
		CleanupInstanceByID(t, env, profile, vmB.GetId())
	})

	readyA := WaitForInstanceState(t, env, profile, vmA.GetId(), "ready")
	readyB := WaitForInstanceState(t, env, profile, vmB.GetId(), "ready")

	ipA := InstanceIP(t, readyA)
	ipB := InstanceIP(t, readyB)
	t.Logf("VM A ready: ip=%s  VM B ready: ip=%s", ipA, ipB)

	sshCfg := TestSSHConfig()

	t.Logf("Sending token A->B: %s -> %s:%s", ipA, ipB, ncPort)
	sendAtoB := fmt.Sprintf("echo %s | nc -w 5 %s %s", tokenAtoB, ipB, ncPort)
	WaitForSSHCommand(t, ipA, sshCfg, sendAtoB, 90*time.Second)

	t.Logf("Sending token B->A: %s -> %s:%s", ipB, ipA, ncPort)
	sendBtoA := fmt.Sprintf("echo %s | nc -w 5 %s %s", tokenBtoA, ipA, ncPort)
	WaitForSSHCommand(t, ipB, sshCfg, sendBtoA, 90*time.Second)

	t.Log("Verifying A->B: reading received file on VM B")
	gotB := WaitForSSHCommand(t, ipB, sshCfg, "cat "+outFile, 30*time.Second)
	if strings.TrimSpace(gotB) != tokenAtoB {
		t.Fatalf("[%s] A->B token mismatch: got %q, want %q", profile.Name, strings.TrimSpace(gotB), tokenAtoB)
	}
	t.Logf("A->B verified: VM B received %q", tokenAtoB)

	t.Log("Verifying B->A: reading received file on VM A")
	gotA := WaitForSSHCommand(t, ipA, sshCfg, "cat "+outFile, 30*time.Second)
	if strings.TrimSpace(gotA) != tokenBtoA {
		t.Fatalf("[%s] B->A token mismatch: got %q, want %q", profile.Name, strings.TrimSpace(gotA), tokenBtoA)
	}
	t.Logf("B->A verified: VM A received %q", tokenBtoA)

	PreserveInstanceArtifacts(t, profile, vmA.GetId())
	PreserveInstanceArtifacts(t, profile, vmB.GetId())
	DeleteAndWait(t, env, profile, vmA.GetId(), ipA)
	DeleteAndWait(t, env, profile, vmB.GetId(), ipB)
}

// RunIPXEBootFlow creates an instance with an iPXE script pointing to the
// test base image served over a local HTTP server, waits for it to reach
// "ready", and verifies the iPXE V1 pipeline:
//   - iPXE script was parsed and the image was downloaded & cached
//   - GRUB bootcmd was injected into the cloud-init userData
//   - The VM booted successfully with the downloaded image
//
// For real VM backends (libvirt, darwin-qemu), the test additionally SSHes in
// and verifies that GRUB was configured with the adapted console settings.
// For mock backends, only artifact-level verification is performed.
func RunIPXEBootFlow(t *testing.T, env *TestEnv, profile RuntimeProfile) {
	t.Helper()
	// iPXE instances use DHCP networking. darwin-qemu doesn't have dnsmasq,
	// so the VM can't get an IP. Only mock (artifact-only) and libvirt (full DHCP) work.
	if profile.Config.EffectiveVMBackend() == VMBackendDarwinQEMU {
		t.Skip("iPXE boot flow requires DHCP infrastructure (dnsmasq); not available on darwin-qemu")
	}
	instanceName := UniqueInstanceName("ipxe-boot")

	// Serve the test base image over HTTP so the iPXE image cache downloads it.
	// For mock backend, create a fake qcow2 file since no real image is needed.
	baseImagePath := profile.Config.BaseImage
	if baseImagePath == "" {
		fakeImage := filepath.Join(t.TempDir(), "fake-base.qcow2")
		if err := os.WriteFile(fakeImage, []byte("QFI\xfb"+"mock-image-data"), 0o644); err != nil {
			t.Fatalf("write fake base image: %v", err)
		}
		baseImagePath = fakeImage
	}

	imageServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Logf("iPXE image server: %s %s", r.Method, r.URL.Path)
		http.ServeFile(w, r, baseImagePath)
	}))
	defer imageServer.Close()

	imageURL := imageServer.URL + "/nke-24.04-x86_64-lvm-2026-02-13-1771001403.qcow2"

	// This iPXE script mirrors the real production FDE profile from
	// profiles/osProfiles/nke-260213-24.04-amd64-fde/ipxeScript
	// with only the image-url replaced by the local test HTTP server.
	ipxeScript := fmt.Sprintf(`#!ipxe
# Generic qcow-imager iPXE script for x86_64 LVM-based cloud images
# This script boots the qcow-imager.efi which writes the cloud image to disk and boots it

# Boot loader and configuration
set boot-loader ${base-url}/internal/x86_64/qcow-imager.efi
# Serial console on ttyS1 (Lenovo/Supermicro COM2)
set console-config console=tty0 console=ttyS1,115200
# Optimized kernel params: loglevel=1 (quiet) for faster boot, iommu settings match image config
set kernel-params pci=realloc=off reboot=pci loglevel=1 iommu=pt intel_iommu=on

# Image URL (substituted for test: pointing to local HTTP server serving base image)
set image-url %s

# LUKS encryption key for first boot (TPM binding removes this after first boot)
set luks-key luks.key=changeit

# Disk and filesystem configuration (LVM layout)
set target-disk /dev/nvme0n1

# Cloud-init datasource
set cloud-init-ds ds=nocloud-net;s=${cloudinit-url}

# Boot with qcow-imager
chain ${boot-loader} ${console-config} ${kernel-params} ${luks-key} image_url=${image-url} image_disk=${target-disk} ${cloud-init-ds} update_grub_template=yes update_grub_cfg=yes
boot
`, imageURL)

	// Build userData that enables SSH access so we can verify GRUB config on real backends
	userData := "#cloud-config\n" +
		"hostname: " + instanceName + "\n" +
		"manage_etc_hosts: true\n" +
		"ssh_pwauth: true\n" +
		"users:\n" +
		"  - name: " + sshUser + "\n" +
		"    plain_text_passwd: " + sshPassword + "\n" +
		"    lock_passwd: false\n" +
		"    sudo: ALL=(ALL) NOPASSWD:ALL\n" +
		"    shell: /bin/bash\n" +
		"runcmd:\n" +
		"  - echo ipxe-boot-ok > /tmp/ipxe-marker\n"

	createPayload := &forgesdk.InstanceCreateRequest{
		Name:           instanceName,
		TenantId:       "d282929f-49d5-4dec-8d27-21ceb30ade19",
		InstanceTypeId: forgesdk.PtrString("itype-mock-001"),
		VpcId:          "vpc-mock-001",
		Labels:         map[string]string{"nke.nvidia.com/node-id": instanceName},
		IpxeScript:     *forgesdk.NewNullableString(forgesdk.PtrString(ipxeScript)),
		UserData:       *forgesdk.NewNullableString(forgesdk.PtrString(userData)),
		Interfaces: []forgesdk.InterfaceCreateRequest{
			{SubnetId: forgesdk.PtrString("subnet-mock-001")},
		},
	}

	created := env.CreateInstance(createPayload)
	t.Logf("[%s] iPXE instance created: id=%s name=%s", profile.Name, created.GetId(), created.GetName())
	t.Cleanup(func() {
		PreserveInstanceArtifacts(t, profile, created.GetId())
		CleanupInstanceByID(t, env, profile, created.GetId())
	})

	readyInstance := WaitForInstanceState(t, env, profile, created.GetId(), "ready")
	t.Logf("[%s] iPXE instance ready", profile.Name)

	// Verify artifacts on disk
	workDir := filepath.Join(filepath.Dir(profile.Config.DBPath), "instances", created.GetId())
	verifyFileExists(t, filepath.Join(workDir, "disk.qcow2"))
	verifyFileExists(t, filepath.Join(workDir, "seed.iso"))

	// Verify user-data includes injected bootcmd
	userDataBytes, err := os.ReadFile(filepath.Join(workDir, "user-data"))
	if err != nil {
		t.Fatalf("failed to read user-data: %v", err)
	}
	userDataStr := string(userDataBytes)
	if !strings.Contains(userDataStr, "bootcmd:") {
		t.Errorf("user-data missing bootcmd injection:\n%s", userDataStr)
	}
	if !strings.Contains(userDataStr, "console=ttyS0") {
		t.Errorf("user-data missing adapted console (ttyS0):\n%s", userDataStr)
	}
	if !strings.Contains(userDataStr, "update-grub") {
		t.Errorf("user-data missing update-grub command:\n%s", userDataStr)
	}
	t.Logf("[%s] iPXE user-data verified: bootcmd injected with console=ttyS0", profile.Name)

	// Verify image was cached
	cacheDir := profile.Config.ImageCacheDirectory()
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		t.Fatalf("failed to read cache dir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("image cache directory is empty")
	}
	t.Logf("[%s] iPXE image cached: %d file(s)", profile.Name, len(entries))

	// For real VM backends, SSH in and verify GRUB config
	if profile.Config.EffectiveVMBackend() != VMBackendMock {
		ip := InstanceIP(t, readyInstance)
		sshCfg := TestSSHConfig()

		// Verify the cloud-init marker was written (proves runcmd ran)
		marker := WaitForSSHCommand(t, ip, sshCfg, "cat /tmp/ipxe-marker", 90*time.Second)
		if strings.TrimSpace(marker) != "ipxe-boot-ok" {
			t.Fatalf("[%s] ipxe-marker mismatch: got %q, want %q", profile.Name, strings.TrimSpace(marker), "ipxe-boot-ok")
		}
		t.Logf("[%s] iPXE SSH marker verified", profile.Name)

		// Verify GRUB was configured by the bootcmd
		grubDefault := WaitForSSHCommand(t, ip, sshCfg, "cat /etc/default/grub", 10*time.Second)
		if !strings.Contains(grubDefault, "console=ttyS0") {
			t.Errorf("[%s] /etc/default/grub missing console=ttyS0:\n%s", profile.Name, grubDefault)
		}
		if !strings.Contains(grubDefault, "loglevel=1") {
			t.Errorf("[%s] /etc/default/grub missing loglevel=1:\n%s", profile.Name, grubDefault)
		}
		t.Logf("[%s] iPXE GRUB config verified via SSH", profile.Name)
	}

	PreserveInstanceArtifacts(t, profile, created.GetId())
	DeleteAndWait(t, env, profile, created.GetId(), "")
	t.Logf("[%s] iPXE instance deleted successfully", profile.Name)
}

// RunIPXEProductionCloudInitFlow boots the production FDE image with a
// rendered copy of the real production userData template. This verifies that
// the cloud-init runcmd pipeline (kubeadm bundle download, OVN install,
// nke-agent restart, etc.) works end-to-end inside our VM environment.
//
// Gated by FORGE_VMS_TEST_PRODUCTION_IMAGE=true (same as the basic production
// image test since it also requires the multi-GB image download).
func RunIPXEProductionCloudInitFlow(t *testing.T, env *TestEnv, profile RuntimeProfile) {
	t.Helper()

	if !IsTrueEnv("FORGE_VMS_TEST_PRODUCTION_IMAGE") {
		t.Skip("skipping production cloud-init test; set FORGE_VMS_TEST_PRODUCTION_IMAGE=true to enable")
	}
	if profile.Config.EffectiveVMBackend() == VMBackendMock {
		t.Skip("production cloud-init test requires a real VM backend")
	}

	instanceName := UniqueInstanceName("ipxe-prod-ci")

	// Read the rendered production userData from testdata.
	userDataBytes, err := os.ReadFile(filepath.Join(TestdataDir(), "prod-userdata-rendered.yaml"))
	if err != nil {
		t.Fatalf("failed to read prod-userdata-rendered.yaml: %v", err)
	}
	userData := string(userDataBytes)
	// Replace hostname placeholder with unique instance name.
	userData = strings.Replace(userData, "hostname: prod-cloudinit-test", "hostname: "+instanceName, 1)

	// Allow overriding the image URL via env var to test custom-built images.
	// Three modes:
	//   - Not set: uses production CloudFront URL
	//   - Full URL (http://...): used as-is (standalone server)
	//   - Filename only: resolved via test server's /files/images/ endpoint
	imageURL := os.Getenv("FORGE_VMS_PROD_IMAGE_URL")
	if imageURL == "" {
		imageURL = ProductionImageURL
	} else if !strings.HasPrefix(imageURL, "http") {
		// Treat as filename — serve from the test server's /files/images/ endpoint.
		imageURL = env.Server.URL + "/files/images/" + imageURL
	}
	t.Logf("Using image URL: %s", imageURL)

	productionAMD64Script := fmt.Sprintf(`#!ipxe
set boot-loader https://d3mq564cn7sth7.cloudfront.net/nke-isos/qcow/efi/x86_64/2026-03-10/1773155704/qcow-imager.efi
set image-url %s
set disk-strategy smallest
set console-config console=tty0 console=ttyS1,115200
set kernel-params pci=realloc=off reboot=pci loglevel=1 iommu=pt intel_iommu=on
set luks-key luks.key=changeit
set cloud-init-ds ds=nocloud-net;s=${cloudinit-url}
chain ${boot-loader} ${console-config} ${kernel-params} ${luks-key} image_url=${image-url} disk_strategy=${disk-strategy} ${cloud-init-ds} update_grub_template=yes update_grub_cfg=yes
boot
`, imageURL)
	createPayload := &forgesdk.InstanceCreateRequest{
		Name:             instanceName,
		TenantId:         "d282929f-49d5-4dec-8d27-21ceb30ade19",
		InstanceTypeId:   forgesdk.PtrString("itype-mock-001"),
		VpcId:            "vpc-mock-001",
		Labels:           map[string]string{"nke.nvidia.com/node-id": instanceName},
		IpxeScript:       *forgesdk.NewNullableString(forgesdk.PtrString(productionAMD64Script)),
		UserData:         *forgesdk.NewNullableString(forgesdk.PtrString(userData)),
		PhoneHomeEnabled: forgesdk.PtrBool(true),
		Interfaces: []forgesdk.InterfaceCreateRequest{
			{SubnetId: forgesdk.PtrString("subnet-mock-001")},
		},
	}

	created := env.CreateInstance(createPayload)
	t.Logf("[%s] production cloud-init instance created: id=%s name=%s (phoneHomeEnabled=true)", profile.Name, created.GetId(), created.GetName())
	t.Cleanup(func() {
		// Always preserve console logs (not just on failure) for cycle analysis.
		printConsoleLogs(t, profile, created.GetId())
		PreserveInstanceArtifacts(t, profile, created.GetId())
		CleanupInstanceByID(t, env, profile, created.GetId())
	})

	prodProfile := profile
	prodProfile.ReadyTimeout = durationEnvOr("FORGE_VMS_TEST_PROD_IMAGE_TIMEOUT", 16*time.Minute)

	readyInstance := WaitForInstanceState(t, env, prodProfile, created.GetId(), "ready")
	ip := InstanceIP(t, readyInstance)
	t.Logf("[%s] production cloud-init instance ready: ip=%s", profile.Name, ip)

	// Verify TPM EK certificate is present in the API response.
	// The libvirt backend pre-creates swtpm state and captures the EK cert
	// during VM creation. It should be base64-encoded DER.
	verifyTPMEKCert(t, profile.Name, readyInstance)

	sshCfg := TestSSHConfig()

	// With PhoneHomeEnabled=true, the instance only reaches "ready" after
	// cloud-init POSTs phone_home — meaning all runcmd steps have completed.
	// No need for cloud-init status --wait; just verify the marker.
	marker := WaitForSSHCommand(t, ip, sshCfg, "cat /tmp/prod-cloudinit-marker", 30*time.Second)
	if strings.TrimSpace(marker) != "prod-cloudinit-ok" {
		t.Fatalf("[%s] marker mismatch: got %q, want %q", profile.Name, strings.TrimSpace(marker), "prod-cloudinit-ok")
	}
	t.Logf("[%s] production cloud-init marker verified", profile.Name)

	// Check cloud-init status and datasource.
	ciStatus := WaitForSSHCommand(t, ip, sshCfg, "cloud-init status --long 2>/dev/null || echo STATUS_UNAVAILABLE", 30*time.Second)
	t.Logf("[%s] cloud-init status:\n%s", profile.Name, ciStatus)

	// Verify kubeadm was installed (the main bundle).
	kubeadmCheck := WaitForSSHCommand(t, ip, sshCfg, "command -v kubeadm && kubeadm version --output=short 2>/dev/null || echo NOT_FOUND", 30*time.Second)
	t.Logf("[%s] kubeadm check: %s", profile.Name, strings.TrimSpace(kubeadmCheck))
	if strings.Contains(kubeadmCheck, "NOT_FOUND") {
		t.Errorf("[%s] kubeadm not found after cloud-init completed", profile.Name)
	}

	// Verify OVN was installed. The bundle installs Helm charts to
	// /etc/dgx/ovn-kubernetes/ and imports container images into containerd.
	// No host-level OVN binaries are installed by the bundle.
	ovnCheck := WaitForSSHCommand(t, ip, sshCfg, "test -d /etc/dgx/ovn-kubernetes/charts && echo OVN_OK || echo OVN_NOT_FOUND", 30*time.Second)
	t.Logf("[%s] OVN check: %s", profile.Name, strings.TrimSpace(ovnCheck))
	if strings.Contains(ovnCheck, "OVN_NOT_FOUND") {
		t.Errorf("[%s] OVN helm charts not found at /etc/dgx/ovn-kubernetes/ after cloud-init completed", profile.Name)
	}

	// Check fetch-install log for errors.
	fetchLog := WaitForSSHCommand(t, ip, sshCfg, "cat /var/log/fetch-install.log 2>/dev/null || echo NO_LOG", 30*time.Second)
	t.Logf("[%s] fetch-install.log:\n%s", profile.Name, fetchLog)

	// Check nke-agent status (may fail, that's expected without full config).
	nkeStatus := WaitForSSHCommand(t, ip, sshCfg, "systemctl is-active nke-agent 2>/dev/null || systemctl status nke-agent --no-pager 2>&1 | head -10", 30*time.Second)
	t.Logf("[%s] nke-agent status: %s", profile.Name, strings.TrimSpace(nkeStatus))

	PreserveInstanceArtifacts(t, profile, created.GetId())
	DeleteAndWait(t, env, profile, created.GetId(), ip)
	t.Logf("[%s] production cloud-init instance deleted successfully", profile.Name)
}

// testdataDir returns the path to the testdata directory relative to this source file.
func TestdataDir() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "testdata")
}

// RunIPXEProductionImageFlow is like RunIPXEBootFlow but uses the verbatim
// production iPXE script from profiles/osProfiles with the real CloudFront
// image URL. The image is multi-GB so this test is skipped unless
// FORGE_VMS_TEST_PRODUCTION_IMAGE=true is set.
//
// Architecture is auto-detected: ARM64 hosts use the aarch64 image, x86_64
// hosts use the amd64 image.
//
// Cloud-init is served via HTTP metadata server at 169.254.169.254:7777.
// The production image already polls this address on boot (EC2-style metadata).
// No seed ISO or grub.cfg patching is needed.
//
// On ARM64 macOS: blocked by nvidia-64k kernel (64KB pages)
// which can't run under HVF (16KB pages).
//
// This test is only meaningful on real VM backends (qemu, libvirt) — mock
// is always skipped since it cannot boot a real image.
func RunIPXEProductionImageFlow(t *testing.T, env *TestEnv, profile RuntimeProfile) {
	t.Helper()

	if !IsTrueEnv("FORGE_VMS_TEST_PRODUCTION_IMAGE") {
		t.Skip("skipping production image test; set FORGE_VMS_TEST_PRODUCTION_IMAGE=true to enable")
	}
	if profile.Config.EffectiveVMBackend() == VMBackendMock {
		t.Skip("production image test requires a real VM backend")
	}
	// The production aarch64 FDE images use the nvidia-64k kernel (64KB pages).
	// macOS HVF only supports 16KB pages, and swapping just the kernel doesn't
	// work because the nvidia-64k initrd's dm-crypt/LVM modules are incompatible
	// with the generic kernel. This test requires a native KVM host.
	if runtime.GOARCH == "arm64" && runtime.GOOS == "darwin" {
		t.Skip("production aarch64 FDE image requires native KVM (nvidia-64k kernel + LUKS modules); macOS HVF cannot run it")
	}

	instanceName := UniqueInstanceName("ipxe-prod")

	// Verbatim production FDE iPXE scripts from profiles/osProfiles.
	// Select based on host architecture so the test runs on both x86_64 and ARM64.
	productionAMD64Script := fmt.Sprintf(`#!ipxe
set boot-loader https://d3mq564cn7sth7.cloudfront.net/nke-isos/qcow/efi/x86_64/2026-03-10/1773155704/qcow-imager.efi
set image-url %s
set disk-strategy {{ .DiskSelectionStrategy }}
set console-config console=tty0 console=ttyS1,115200
set kernel-params pci=realloc=off reboot=pci loglevel=1 iommu=pt intel_iommu=on
set luks-key luks.key=changeit
set cloud-init-ds ds=nocloud-net;s=${cloudinit-url}
chain ${boot-loader} ${console-config} ${kernel-params} ${luks-key} image_url=${image-url} disk_strategy=${disk-strategy} ${cloud-init-ds} update_grub_template=yes update_grub_cfg=yes
boot
`, ProductionImageURL)
	const productionARM64Script = `#!ipxe
set boot-loader https://d3mq564cn7sth7.cloudfront.net/nke-isos/qcow/efi/aarch64/2026-03-06/1772834088/qcow-imager.efi
set image-url https://d3mq564cn7sth7.cloudfront.net/nke-isos/qcow/images/24.04/aarch64/lvm/2026-03-04/1772595122/nke-24.04-aarch64-lvm-2026-03-04-1772595122.qcow2
set disk-strategy {{ .DiskSelectionStrategy }}
set console-config console=ttyAMA0,115200
set kernel-params ip=dhcp fb=false pci=realloc=off loglevel=1 iommu=pt
set luks-key luks.key=changeit
set cloud-init-ds ds=nocloud-net;s=${cloudinit-url}
chain ${boot-loader} ${console-config} ${kernel-params} ${luks-key} image_url=${image-url} disk_strategy=${disk-strategy} ${cloud-init-ds} update_grub_template=yes update_grub_cfg=yes
boot
`

	var productionIPXEScript string
	if runtime.GOARCH == "arm64" {
		productionIPXEScript = productionARM64Script
		t.Logf("[%s] using ARM64 production iPXE script (aarch64 qcow2)", profile.Name)
	} else {
		productionIPXEScript = productionAMD64Script
		t.Logf("[%s] using AMD64 production iPXE script (x86_64 qcow2)", profile.Name)
	}

	userData := "#cloud-config\n" +
		"hostname: " + instanceName + "\n" +
		"manage_etc_hosts: true\n" +
		"ssh_pwauth: true\n" +
		"users:\n" +
		"  - name: " + sshUser + "\n" +
		"    plain_text_passwd: " + sshPassword + "\n" +
		"    lock_passwd: false\n" +
		"    sudo: ALL=(ALL) NOPASSWD:ALL\n" +
		"    shell: /bin/bash\n" +
		"runcmd:\n" +
		"  - echo ipxe-prod-ok > /tmp/ipxe-marker\n" +
		PhoneHomeYAML

	createPayload := &forgesdk.InstanceCreateRequest{
		Name:             instanceName,
		TenantId:         "d282929f-49d5-4dec-8d27-21ceb30ade19",
		InstanceTypeId:   forgesdk.PtrString("itype-mock-001"),
		VpcId:            "vpc-mock-001",
		Labels:           map[string]string{"nke.nvidia.com/node-id": instanceName},
		IpxeScript:       *forgesdk.NewNullableString(forgesdk.PtrString(productionIPXEScript)),
		UserData:         *forgesdk.NewNullableString(forgesdk.PtrString(userData)),
		PhoneHomeEnabled: forgesdk.PtrBool(true),
		Interfaces: []forgesdk.InterfaceCreateRequest{
			{SubnetId: forgesdk.PtrString("subnet-mock-001")},
		},
	}

	created := env.CreateInstance(createPayload)
	t.Logf("[%s] production iPXE instance created: id=%s name=%s (phoneHomeEnabled=true)", profile.Name, created.GetId(), created.GetName())
	t.Cleanup(func() {
		printConsoleLogs(t, profile, created.GetId())
		PreserveInstanceArtifacts(t, profile, created.GetId())
		CleanupInstanceByID(t, env, profile, created.GetId())
	})

	// Use a longer timeout — the production image is multi-GB and the
	// download from CloudFront can take several minutes.
	prodProfile := profile
	prodProfile.ReadyTimeout = durationEnvOr("FORGE_VMS_TEST_PROD_IMAGE_TIMEOUT", 16*time.Minute)

	readyInstance := WaitForInstanceState(t, env, prodProfile, created.GetId(), "ready")
	ip := InstanceIP(t, readyInstance)
	t.Logf("[%s] production iPXE instance ready: ip=%s", profile.Name, ip)

	// Verify TPM EK certificate is present in the API response.
	verifyTPMEKCert(t, profile.Name, readyInstance)

	// Verify image was cached
	cacheDir := profile.Config.ImageCacheDirectory()
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		t.Fatalf("failed to read cache dir: %v", err)
	}
	found := false
	for _, e := range entries {
		info, infoErr := e.Info()
		if infoErr == nil && info.Size() > 1_000_000_000 { // production images are multi-GB
			found = true
			t.Logf("[%s] production image cached: %s (%d bytes)", profile.Name, e.Name(), info.Size())
		}
	}
	if !found {
		t.Errorf("no large cached image found in %s (expected multi-GB production qcow2)", cacheDir)
	}

	// SSH in and verify.
	// Production images under TCG emulation take several minutes for cloud-init
	// modules:final to complete (LUKS resize, TPM binding, runcmd).
	sshCfg := TestSSHConfig()

	marker := WaitForSSHCommand(t, ip, sshCfg, "cat /tmp/ipxe-marker", 5*time.Minute)
	if strings.TrimSpace(marker) != "ipxe-prod-ok" {
		t.Fatalf("[%s] marker mismatch: got %q, want %q", profile.Name, strings.TrimSpace(marker), "ipxe-prod-ok")
	}
	t.Logf("[%s] production iPXE SSH marker verified", profile.Name)

	grubDefault := WaitForSSHCommand(t, ip, sshCfg, "cat /etc/default/grub", 10*time.Second)
	if !strings.Contains(grubDefault, "console=ttyS0") {
		t.Errorf("[%s] /etc/default/grub missing console=ttyS0:\n%s", profile.Name, grubDefault)
	}
	if !strings.Contains(grubDefault, "loglevel=1") {
		t.Errorf("[%s] /etc/default/grub missing loglevel=1:\n%s", profile.Name, grubDefault)
	}
	t.Logf("[%s] production iPXE GRUB config verified via SSH:\n%s", profile.Name, grubDefault)

	// Dump cloud-init diagnostics for debugging datasource issues.
	cmdline := WaitForSSHCommand(t, ip, sshCfg, "cat /proc/cmdline", 10*time.Second)
	t.Logf("[%s] kernel cmdline:\n%s", profile.Name, cmdline)

	cloudCfg := WaitForSSHCommand(t, ip, sshCfg, "cat /etc/cloud/cloud.cfg 2>/dev/null || echo NOT_FOUND", 10*time.Second)
	t.Logf("[%s] /etc/cloud/cloud.cfg:\n%s", profile.Name, cloudCfg)

	cloudCfgD := WaitForSSHCommand(t, ip, sshCfg, "ls -la /etc/cloud/cloud.cfg.d/ 2>/dev/null && cat /etc/cloud/cloud.cfg.d/*.cfg 2>/dev/null || echo NO_DROP_INS", 10*time.Second)
	t.Logf("[%s] /etc/cloud/cloud.cfg.d/:\n%s", profile.Name, cloudCfgD)

	ciStatus := WaitForSSHCommand(t, ip, sshCfg, "cloud-init status --long 2>/dev/null || echo STATUS_UNAVAILABLE", 10*time.Second)
	t.Logf("[%s] cloud-init status:\n%s", profile.Name, ciStatus)

	// Verify console log is accessible via SSH console URL.
	prodConsoleURL := readyInstance.GetSerialConsoleUrl()
	if prodConsoleURL == "" {
		t.Fatal("SerialConsoleUrl not set on ready production instance")
	}
	t.Logf("SerialConsoleUrl: %s", prodConsoleURL)
	prodConsoleOutput := FetchConsoleLogViaSSH(t, prodConsoleURL)
	if len(prodConsoleOutput) == 0 {
		t.Fatal("console log via SSH returned empty output for production image")
	}
	t.Logf("Console log via SSH (production): %d bytes", len(prodConsoleOutput))

	PreserveInstanceArtifacts(t, profile, created.GetId())
	DeleteAndWait(t, env, profile, created.GetId(), ip)
	t.Logf("[%s] production iPXE instance deleted successfully", profile.Name)
}

// printConsoleLogs copies the VM console logs to the test output on failure.
// Checks both ttyS0 (cloud images) and ttyS1 (production bare-metal images).
// Also reads virtlogd rotated files (.log.0, .log.1, .log.2) to capture the
// complete boot history even when logs exceed the virtlogd max_size.
func printConsoleLogs(t *testing.T, profile RuntimeProfile, instanceID string) {
	t.Helper()
	if !t.Failed() {
		return
	}
	instanceDir := filepath.Join(filepath.Dir(profile.Config.DBPath), "instances", instanceID)
	for _, suffix := range []string{"disk.qcow2.ttyS0.log", "disk.qcow2.ttyS1.log", "disk.qcow2.console.log"} {
		consolePath := filepath.Join(instanceDir, suffix)

		// Read rotated files first (oldest to newest), then current.
		var combined []byte
		for i := 2; i >= 0; i-- {
			if data, err := os.ReadFile(fmt.Sprintf("%s.%d", consolePath, i)); err == nil {
				combined = append(combined, data...)
			}
		}
		if data, err := os.ReadFile(consolePath); err == nil {
			combined = append(combined, data...)
		}
		if len(combined) == 0 {
			continue
		}

		// Strip non-printable characters for readable output
		var clean []byte
		for _, b := range combined {
			if b >= 32 || b == '\n' || b == '\r' || b == '\t' {
				clean = append(clean, b)
			}
		}
		t.Logf("=== VM Console Log %s (%d bytes) ===\n%s\n=== End Console Log ===", suffix, len(combined), string(clean))
	}
}

func verifyFileExists(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Errorf("expected file %s to exist: %v", path, err)
		return
	}
	if info.Size() == 0 {
		t.Errorf("file %s is empty", path)
	}
}

// RunSSHVerificationFlow creates an instance with cloud-init UserData that
// enables password-based SSH and writes a test marker, waits for it to reach
// "ready" with an IP, then verifies full SSH connectivity by reading back the
// marker file written by cloud-init's runcmd.
func RunSSHVerificationFlow(t *testing.T, env *TestEnv, profile RuntimeProfile) {
	t.Helper()
	instanceName := UniqueInstanceName("ssh-verify-test")

	userData := "#cloud-config\n" +
		"hostname: " + instanceName + "\n" +
		"manage_etc_hosts: true\n" +
		"ssh_pwauth: true\n" +
		"users:\n" +
		"  - name: testuser\n" +
		"    plain_text_passwd: testpass\n" +
		"    lock_passwd: false\n" +
		"    sudo: ALL=(ALL) NOPASSWD:ALL\n" +
		"    shell: /bin/bash\n" +
		"runcmd:\n" +
		"  - echo integration-test-ok > /tmp/forge-vms-test-marker\n"

	createPayload := &forgesdk.InstanceCreateRequest{
		Name:           instanceName,
		TenantId:       "d282929f-49d5-4dec-8d27-21ceb30ade19",
		InstanceTypeId: forgesdk.PtrString("itype-mock-001"),
		VpcId:          "vpc-mock-001",
		UserData:       *forgesdk.NewNullableString(forgesdk.PtrString(userData)),
		Labels:         map[string]string{"nke.nvidia.com/node-id": instanceName},
		Interfaces: []forgesdk.InterfaceCreateRequest{
			{SubnetId: forgesdk.PtrString("subnet-mock-001")},
		},
	}

	created := env.CreateInstance(createPayload)
	t.Logf("Instance created: id=%s name=%s", created.GetId(), created.GetName())
	t.Cleanup(func() {
		PreserveInstanceArtifacts(t, profile, created.GetId())
		CleanupInstanceByID(t, env, profile, created.GetId())
	})

	readyInst := WaitForInstanceState(t, env, profile, created.GetId(), "ready")
	ip := InstanceIP(t, readyInst)
	t.Logf("Instance ready with IP: %s", ip)

	t.Log("Verifying SSH connectivity by reading cloud-init marker...")
	sshCfg := TestSSHConfig()
	got := WaitForSSHCommand(t, ip, sshCfg, "cat /tmp/forge-vms-test-marker", 90*time.Second)
	const wantMarker = "integration-test-ok"
	if strings.TrimSpace(got) != wantMarker {
		t.Fatalf("marker mismatch: got %q, want %q", strings.TrimSpace(got), wantMarker)
	}
	t.Logf("SSH command succeeded on %s: marker verified", ip)

	// Verify console log is accessible via SSH console URL.
	consoleURL := readyInst.GetSerialConsoleUrl()
	if consoleURL == "" {
		t.Fatal("SerialConsoleUrl not set on ready instance")
	}
	t.Logf("SerialConsoleUrl: %s", consoleURL)
	consoleOutput := FetchConsoleLogViaSSH(t, consoleURL)
	if len(consoleOutput) == 0 {
		t.Fatal("console log via SSH returned empty output")
	}
	t.Logf("Console log via SSH: %d bytes", len(consoleOutput))

	PreserveInstanceArtifacts(t, profile, created.GetId())
}

// verifyTPMEKCert checks that the Forge API response contains a non-empty,
// valid base64-encoded TPM EK certificate. This is only expected on the
// libvirt backend where swtpm is available.
func verifyTPMEKCert(t *testing.T, profileName string, inst forgesdk.Instance) {
	t.Helper()

	cert := inst.GetTpmEkCertificate()
	if cert == "" {
		t.Errorf("[%s] instance %s: tpmEkCertificate is empty in API response", profileName, inst.GetId())
		return
	}

	// Verify it's valid base64.
	decoded, err := base64.StdEncoding.DecodeString(cert)
	if err != nil {
		t.Errorf("[%s] instance %s: tpmEkCertificate is not valid base64: %v", profileName, inst.GetId(), err)
		return
	}

	t.Logf("[%s] instance %s: tpmEkCertificate present (%d bytes decoded, %d bytes base64)",
		profileName, inst.GetId(), len(decoded), len(cert))
}
