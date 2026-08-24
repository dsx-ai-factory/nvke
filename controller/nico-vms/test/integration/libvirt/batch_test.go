//go:build integration

// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package libvirt

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	forgesdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"

	"github.com/NVIDIA/nke/controller/nico-vms/test/integration/testutil"
)

// TestBatchProductionCloudInit runs N production cloud-init instances in parallel
// to collect statistics on systemd ordering cycle behavior.
//
// Each instance boots the production FDE image with real cloud-init (kubeadm + OVN).
// Uses PhoneHomeEnabled so "ready" means cloud-init finished everything.
// Console logs are preserved for EVERY instance regardless of outcome.
//
// Usage:
//
//	NICO_VMS_TEST_PRODUCTION_IMAGE=true \
//	NICO_VMS_BATCH_COUNT=30 \
//	NICO_VMS_PRESERVE_LOGS=true \
//	NICO_VMS_RECONCILE_WORKERS=8 \
//	NICO_VMS_READINESS_TIMEOUT=30m \
//	go test -tags=integration -timeout 180m -v ./test/integration/libvirt -run TestBatchProductionCloudInit
//
// After the run, analyze console logs:
//
//	for f in /tmp/nico-vms-preserved/TestBatchProductionCloudInit/*/disk.qcow2.ttyS1.log; do
//	  id=$(basename $(dirname "$f"))
//	  cycle=$(strings "$f" | grep "deleted to break" | head -1)
//	  ds=$(strings "$f" | grep "Datasource Data" | head -1)
//	  printf "%-40s CYCLE: %s\n" "$id" "${cycle:-none}"
//	  printf "%-40s DS:    %s\n" "" "${ds:-unknown}"
//	done
func TestBatchProductionCloudInit(t *testing.T) {
	env, profile := sharedSuite(t)

	if !testutil.IsTrueEnv("NICO_VMS_TEST_PRODUCTION_IMAGE") {
		t.Skip("set NICO_VMS_TEST_PRODUCTION_IMAGE=true")
	}

	count := 5
	if v := os.Getenv("NICO_VMS_BATCH_COUNT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			count = n
		}
	}

	t.Logf("Running %d production cloud-init instances in parallel", count)

	// Read the rendered production userData.
	userDataBytes, err := os.ReadFile(testutil.TestdataDir() + "/prod-userdata-rendered.yaml")
	if err != nil {
		t.Fatalf("failed to read prod-userdata-rendered.yaml: %v", err)
	}
	baseUserData := string(userDataBytes)

	imageURL := os.Getenv("NICO_VMS_PROD_IMAGE_URL")
	if imageURL == "" {
		imageURL = testutil.ProductionImageURL
	} else if !strings.HasPrefix(imageURL, "http") {
		imageURL = env.Server.URL + "/files/images/" + imageURL
	}
	t.Logf("Using image URL: %s", imageURL)

	ipxeScript := fmt.Sprintf(`#!ipxe
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

	for i := 0; i < count; i++ {
		t.Run(fmt.Sprintf("vm-%02d", i), func(t *testing.T) {
			t.Parallel()

			instanceName := testutil.UniqueInstanceName(fmt.Sprintf("batch-%02d", i))
			userData := strings.Replace(baseUserData, "hostname: prod-cloudinit-test", "hostname: "+instanceName, 1)

			createPayload := &forgesdk.InstanceCreateRequest{
				Name:             instanceName,
				TenantId:         "d282929f-49d5-4dec-8d27-21ceb30ade19",
				InstanceTypeId:   forgesdk.PtrString("itype-mock-001"),
				VpcId:            "vpc-mock-001",
				Labels:           map[string]string{"nke.nvidia.com/node-id": instanceName},
				IpxeScript:       *forgesdk.NewNullableString(forgesdk.PtrString(ipxeScript)),
				UserData:         *forgesdk.NewNullableString(forgesdk.PtrString(userData)),
				PhoneHomeEnabled: forgesdk.PtrBool(true),
				Interfaces: []forgesdk.InterfaceCreateRequest{
					{SubnetId: forgesdk.PtrString("subnet-mock-001")},
				},
			}

			tenv := env.WithT(t)
			created := tenv.CreateInstance(createPayload)
			instanceID := created.GetId()
			t.Logf("created: id=%s", instanceID)

			t.Cleanup(func() {
				testutil.PreserveInstanceArtifacts(t, profile, instanceID)
				testutil.CleanupInstanceByID(t, tenv, profile, instanceID)
			})

			// Long timeout: 30 VMs downloading bundles concurrently.
			batchProfile := profile
			batchProfile.ReadyTimeout = 30 * time.Minute

			readyInst := testutil.WaitForInstanceState(t, tenv, batchProfile, instanceID, "ready")
			ip := testutil.InstanceIP(t, readyInst)
			t.Logf("ready: ip=%s", ip)

			sshCfg := testutil.TestSSHConfig()
			marker := testutil.WaitForSSHCommand(t, ip, sshCfg, "cat /tmp/prod-cloudinit-marker", 30*time.Second)
			if strings.TrimSpace(marker) != "prod-cloudinit-ok" {
				t.Errorf("marker mismatch: got %q", strings.TrimSpace(marker))
			}

			ciStatus := testutil.WaitForSSHCommand(t, ip, sshCfg, "cloud-init status --long 2>/dev/null || echo unavailable", 10*time.Second)
			t.Logf("cloud-init status:\n%s", ciStatus)
		})
	}
}
