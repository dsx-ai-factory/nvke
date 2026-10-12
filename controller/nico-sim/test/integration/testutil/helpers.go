// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package testutil

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	forgesdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"

	nvmconfig "github.com/dsx-ai-factory/nvke/controller/nico-sim/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/nico-sim/internal/seed"
)

var instanceNameSeq atomic.Uint64

func UniqueInstanceName(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, instanceNameSeq.Add(1))
}

func ListInstances(t *testing.T, env *TestEnv) []forgesdk.Instance {
	t.Helper()

	status, body := env.Do(http.MethodGet, env.Endpoint("instance")+"?siteId=site-mock-001&pageSize=100&pageNumber=1&orderBy=CREATED_ASC", nil)
	if status != http.StatusOK {
		t.Fatalf("list instances status = %d, want 200", status)
	}

	var instances []forgesdk.Instance
	if err := json.Unmarshal(body, &instances); err != nil {
		t.Fatalf("unmarshal list response failed: %v", err)
	}
	return instances
}

func WaitForInstanceState(
	t *testing.T,
	env *TestEnv,
	profile RuntimeProfile,
	instanceID string,
	wantState string,
) forgesdk.Instance {
	t.Helper()

	lastStatus := "<not-found>"
	deadline := time.Now().Add(profile.ReadyTimeout)
	for time.Now().Before(deadline) {
		for _, inst := range ListInstances(t, env) {
			if inst.GetId() != instanceID {
				continue
			}
			lastStatus = string(inst.GetStatus())
			if strings.EqualFold(string(inst.GetStatus()), "error") && !strings.EqualFold(wantState, "error") {
				t.Fatalf("[%s] instance %s entered error while waiting for state %q: reason=%s",
					profile.Name, instanceID, wantState, env.InstanceErrorReason(inst.GetId()))
			}
			if strings.EqualFold(string(inst.GetStatus()), wantState) {
				// The API maps internal "running" to SDK "Ready" (SDK has no
				// "Running" status). When waiting for "ready", also require IPs
				// to be populated — IPs only appear when ObservedState is truly
				// "ready" (SSH probe passed), not just "running" (VM started).
				if strings.EqualFold(wantState, "ready") {
					ifaces := inst.GetInterfaces()
					if len(ifaces) == 0 || len(ifaces[0].GetIpAddresses()) == 0 {
						continue // still "running", not actually ready
					}
				}
				return inst
			}
		}
		time.Sleep(profile.PollInterval)
	}

	t.Fatalf("[%s] instance %s did not reach state %q (last status=%q)", profile.Name, instanceID, wantState, lastStatus)
	return forgesdk.Instance{}
}

// KeepVMs returns true when NICO_VMS_E2E_TESTS_KEEP_VMS is set.
// When true, tests skip instance deletion so VMs remain running for manual inspection.
func KeepVMs() bool {
	return IsTrueEnv("NICO_VMS_E2E_TESTS_KEEP_VMS")
}

// DeleteAndWait deletes an instance and waits for removal, unless NICO_VMS_E2E_TESTS_KEEP_VMS is set.
// When keeping VMs, logs the instance ID and IP for manual SSH access.
func DeleteAndWait(t *testing.T, env *TestEnv, profile RuntimeProfile, instanceID, ip string) {
	t.Helper()
	if KeepVMs() {
		t.Logf("NICO_VMS_E2E_TESTS_KEEP_VMS: keeping instance %s running (ip=%s). Delete manually when done.", instanceID, ip)
		return
	}
	env.DeleteInstance(instanceID)
	WaitForInstanceDeletion(t, env, profile, instanceID)
}

func CleanupInstanceByID(t *testing.T, env *TestEnv, profile RuntimeProfile, instanceID string) {
	t.Helper()
	if KeepVMs() {
		return // VM kept for inspection; user must delete manually.
	}

	deadline := time.Now().Add(profile.ReadyTimeout)
	endpoint := env.Endpoint("instance/" + instanceID)
	for time.Now().Before(deadline) {
		status, _ := env.Do(http.MethodDelete, endpoint, &forgesdk.InstanceDeleteRequest{})
		switch status {
		case http.StatusOK, http.StatusNotFound:
			return
		default:
			t.Logf("cleanup delete for instance %s returned status %d; retrying", instanceID, status)
		}
		time.Sleep(profile.PollInterval)
	}

	t.Fatalf("[%s] failed to cleanup instance %s before timeout", profile.Name, instanceID)
}

// WaitForInstanceStateChange polls until the instance transitions away from
// currentState. This is used to avoid races where, e.g., a reboot is requested
// but the instance is still in "ready" from before the reboot.
func WaitForInstanceStateChange(
	t *testing.T,
	env *TestEnv,
	profile RuntimeProfile,
	instanceID string,
	currentState string,
) {
	t.Helper()

	deadline := time.Now().Add(profile.ReadyTimeout)
	for time.Now().Before(deadline) {
		for _, inst := range ListInstances(t, env) {
			if inst.GetId() != instanceID {
				continue
			}
			if strings.EqualFold(string(inst.GetStatus()), "error") {
				t.Fatalf("[%s] instance %s entered error while waiting to leave state %q: reason=%s",
					profile.Name, instanceID, currentState, env.InstanceErrorReason(inst.GetId()))
			}
			if !strings.EqualFold(string(inst.GetStatus()), currentState) {
				t.Logf("Instance %s transitioned from %q to %q", instanceID, currentState, string(inst.GetStatus()))
				return
			}
		}
		time.Sleep(profile.PollInterval)
	}

	t.Fatalf("[%s] instance %s did not leave state %q before timeout", profile.Name, instanceID, currentState)
}

// WaitForInstanceError polls until the instance reaches the "error" state.
func WaitForInstanceError(t *testing.T, env *TestEnv, profile RuntimeProfile, instanceID string) forgesdk.Instance {
	t.Helper()

	deadline := time.Now().Add(profile.ReadyTimeout)
	for time.Now().Before(deadline) {
		for _, inst := range ListInstances(t, env) {
			if inst.GetId() != instanceID {
				continue
			}
			if strings.EqualFold(string(inst.GetStatus()), "error") {
				return inst
			}
		}
		time.Sleep(profile.PollInterval)
	}

	t.Fatalf("[%s] instance %s did not reach error state before timeout", profile.Name, instanceID)
	return forgesdk.Instance{}
}

// WaitForInstanceDeletion polls DELETE until a 404 confirms the instance is gone.
func WaitForInstanceDeletion(t *testing.T, env *TestEnv, profile RuntimeProfile, instanceID string) {
	t.Helper()

	deadline := time.Now().Add(profile.ReadyTimeout)
	for time.Now().Before(deadline) {
		status, _ := env.Do(http.MethodDelete, env.Endpoint("instance/"+instanceID), &forgesdk.InstanceDeleteRequest{})
		if status == http.StatusNotFound {
			return
		}
		time.Sleep(profile.PollInterval)
	}
	t.Fatalf("[%s] instance %s was not removed before timeout", profile.Name, instanceID)
}

// SSHProbe attempts a TCP connection to port 22 on the given host.
func SSHProbe(host string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, "22"), timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func MustParseConfig(t *testing.T) nvmconfig.Config {
	t.Helper()
	cfg, err := nvmconfig.Parse(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		t.Fatalf("nvmconfig.Parse failed: %v", err)
	}
	return cfg
}

func SetDefaultEnv(t *testing.T, key, def string) {
	t.Helper()
	if os.Getenv(key) == "" {
		t.Setenv(key, def)
	}
}

func DurationEnvOrDefault(t *testing.T, key string, def time.Duration) time.Duration {
	t.Helper()
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		t.Fatalf("invalid duration for %s=%q: %v", key, raw, err)
	}
	return v
}

func IsTrueEnv(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func IsRunningInsideLima() bool {
	return IsTrueEnv("NICO_VMS_IN_LIMA")
}

func NewRealVMScratchDir(t *testing.T) string {
	t.Helper()

	rootDir := filepath.Join(os.TempDir(), "nico-vms-integration")
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		t.Fatalf("create real VM scratch root failed: %v", err)
	}

	testDir := filepath.Join(
		rootDir,
		fmt.Sprintf("%s-%d", sanitizePathComponent(t.Name()), time.Now().UnixNano()),
	)
	if err := os.MkdirAll(testDir, 0o755); err != nil {
		t.Fatalf("create real VM scratch dir failed: %v", err)
	}
	return testDir
}

func sanitizePathComponent(name string) string {
	replacer := strings.NewReplacer("/", "_", "\\", "_", " ", "_", ":", "_")
	return replacer.Replace(name)
}

// preserveInstanceArtifacts copies VM console logs, cloud-init files, and any
// .log files from the instance working directory to a persistent location under
// /tmp/nico-vms-preserved/{testName}/{instanceID}/. This is triggered when
// NICO_VMS_PRESERVE_LOGS=true is set, allowing inspection of artifacts even
// when tests pass.
func PreserveInstanceArtifacts(t *testing.T, profile RuntimeProfile, instanceID string) {
	t.Helper()

	if !IsTrueEnv("NICO_VMS_PRESERVE_LOGS") {
		return
	}

	instanceDir := filepath.Join(filepath.Dir(profile.Config.DBPath), "instances", instanceID)
	if _, err := os.Stat(instanceDir); os.IsNotExist(err) {
		t.Logf("preserveInstanceArtifacts: instance dir does not exist: %s", instanceDir)
		return
	}

	destDir := filepath.Join("/tmp", "nico-vms-preserved", sanitizePathComponent(t.Name()), instanceID)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Logf("preserveInstanceArtifacts: failed to create destination dir %s: %v", destDir, err)
		return
	}

	// Named files to preserve: console logs and cloud-init artifacts.
	namedFiles := []string{
		"disk.qcow2.ttyS0.log",
		"disk.qcow2.ttyS1.log",
		"disk.qcow2.console.log",
		"user-data",
		"meta-data",
		"network-config",
	}

	var copied int
	for _, name := range namedFiles {
		src := filepath.Join(instanceDir, name)
		if copyFileIfExists(t, src, filepath.Join(destDir, name)) {
			copied++
		}
	}

	// Also copy any other .log files and virtlogd rotated logs (.log.0, .log.1, etc.)
	// in the instance directory.
	entries, err := os.ReadDir(instanceDir)
	if err != nil {
		t.Logf("preserveInstanceArtifacts: failed to read instance dir: %v", err)
	} else {
		seen := make(map[string]bool, len(namedFiles))
		for _, n := range namedFiles {
			seen[n] = true
		}
		for _, entry := range entries {
			if entry.IsDir() || seen[entry.Name()] {
				continue
			}
			if strings.Contains(entry.Name(), ".log") {
				src := filepath.Join(instanceDir, entry.Name())
				if copyFileIfExists(t, src, filepath.Join(destDir, entry.Name())) {
					copied++
				}
			}
		}
	}

	if copied > 0 {
		t.Logf("NICO_VMS_PRESERVE_LOGS: preserved %d artifact(s) to %s", copied, destDir)
	} else {
		t.Logf("NICO_VMS_PRESERVE_LOGS: no artifacts found to preserve in %s", instanceDir)
	}
}

// copyFileIfExists copies src to dst if src exists and is non-empty. Returns
// true if the copy was successful.
func copyFileIfExists(t *testing.T, src, dst string) bool {
	t.Helper()

	srcFile, err := os.Open(src)
	if err != nil {
		return false
	}
	defer srcFile.Close()

	info, err := srcFile.Stat()
	if err != nil || info.Size() == 0 {
		return false
	}

	dstFile, err := os.Create(dst)
	if err != nil {
		t.Logf("preserveInstanceArtifacts: failed to create %s: %v", dst, err)
		return false
	}
	defer dstFile.Close()

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		t.Logf("preserveInstanceArtifacts: failed to copy %s: %v", src, err)
		return false
	}
	return true
}

func DefaultSeedDoc() seed.Document {
	return seed.Document{
		Tenants: []seed.Tenant{
			{ID: "d282929f-49d5-4dec-8d27-21ceb30ade19", OrgID: "novxwn552ssc", DisplayName: "Forge-Tenant-NKE"},
		},
		Sites: []seed.Site{
			{
				ID: "site-mock-001", Name: "mock-site-local", TenantID: "d282929f-49d5-4dec-8d27-21ceb30ade19",
				Capabilities: struct {
					NativeNetworking bool `yaml:"native_networking"`
				}{NativeNetworking: true},
			},
		},
		VPCs:                 []seed.VPC{{ID: "vpc-mock-001", Name: "mock-vpc", TenantID: "d282929f-49d5-4dec-8d27-21ceb30ade19", SiteID: "site-mock-001"}},
		Subnets:              []seed.Subnet{{ID: "subnet-mock-001", Name: "mock-subnet", VPCID: "vpc-mock-001", SiteID: "site-mock-001"}},
		VPCPrefixes:          []seed.VPCPrefix{{ID: "vpcpfx-mock-001", Name: "mock-vpc-prefix", VPCID: "vpc-mock-001", SiteID: "site-mock-001", PrefixLength: 29}},
		InstanceTypes:        []seed.InstanceType{{ID: "itype-mock-001", Name: "cpu2-2gb", SiteID: "site-mock-001", GPUCount: 0, CPUCount: 2, MemoryGiB: 2}},
		SSHKeyGroups:         []seed.SSHKeyGroup{{ID: "sshkg-mock-001", Name: "mock-ssh-keys", SiteIDs: []string{"site-mock-001"}}},
		IPBlocks:             []seed.IPBlock{{ID: "ipblk-mock-001", Name: "mock-ip-block", SiteID: "site-mock-001"}},
		InfiniBandPartitions: []seed.InfiniBandPartition{{ID: "ibpart-mock-001", Name: "mock-ib-partition", SiteID: "site-mock-001"}},
	}
}
