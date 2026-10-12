//go:build kubeadm_bringup

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubeadm

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dsx-ai-factory/nvke/controller/test/kubeadm/internal/cluster"
)

// The GitLab original was a CLI (`cluster-bringup full …`) and took these as
// flags. A `go test` binary has no flag surface of its own, so every former
// flag becomes an NKE_BRINGUP_* environment variable with the same default.
//
// The prefix is deliberately NOT NICO_VMS_*: that namespace belongs to the
// nico-sim binary itself, which parses its own environment. Mixing test-driver
// settings into it would make it impossible to tell which process owns a
// given variable.
const (
	// How the VMs get kubeadm, the kubelet and nvke-agent.
	//
	// "community" fetches upstream releases and an nvke-agent .deb built from
	// this checkout, so a run needs no NVIDIA network. It is the default
	// because the NVKE apt repository is behind a source-IP allow list that
	// GitHub runners are outside of, which left this suite skipped rather than
	// run. "apt" is the NVKE apt repository path, for a host that can reach it.
	provisionCommunity = "community"
	provisionApt       = "apt"

	defaultProvisionScript = "scripts/install-k8s-community.sh"

	// Published production NVKE apt repo. Reachable anonymously over plain
	// HTTPS, which matters because CI runners have no route to NVIDIA's
	// internal network.
	defaultAptRepoURL = "https://nke-prod-assets.s3.us-west-2.amazonaws.com/nvke-apt-noble"
	defaultAptGpgURL  = "https://nke-prod-assets.s3.us-west-2.amazonaws.com/nvke-apt-public.key"
	defaultAptSuite   = "nvke-noble" // NOT plain "noble"
	defaultAptComp    = "main"

	// nico-vms's own default bridge subnet.
	defaultBridgeCIDR = "172.30.0.0/24"

	// Non-overlapping with nico-vms's own bridge subnet (172.30.0.0/24).
	defaultPodCIDR     = "10.244.0.0/16"
	defaultServiceCIDR = "10.96.0.0/12"

	defaultHTTPAddr = ":18080"

	// Generous relative to the GitLab defaults (15m init, 10m join). CI has
	// no /dev/kvm, so guests run under QEMU TCG emulation at roughly an order
	// of magnitude slower than native; the stock timeouts are calibrated for
	// KVM and are the most likely cause of a spurious red run.
	defaultInitTimeout = 45 * time.Minute
	defaultJoinTimeout = 30 * time.Minute

	// The CNI install fetches manifests and waits for the Tigera operator to
	// create its CRDs, which means an image pull on an emulated guest.
	defaultCNITimeout = 20 * time.Minute
	// Readiness cannot reuse the GitLab suite's 5m: their CNI images shipped
	// pre-embedded in Debian packages, so their budget covered pod scheduling
	// only. Ours pulls from a public registry under emulation first.
	defaultReadyTimeout = 30 * time.Minute
)

// config is the fully resolved test configuration.
type config struct {
	nicoVMSBin   string
	seedPath     string
	templatePath string

	provisionMode   string
	provisionScript string
	agentDeb        string

	kubeadmDeb        string
	kubeadmDebVersion string
	sshPasswordHash   string

	aptRepoURL, aptGpgURL, aptSuite, aptComp string

	bridgeName, bridgeCIDR string
	podCIDR, serviceCIDR   string

	nodeImageURL string
	workDir      string

	initTimeout  time.Duration
	joinTimeout  time.Duration
	cniTimeout   time.Duration
	readyTimeout time.Duration

	calicoVersion string

	keepVMs bool
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envDuration(t *testing.T, key string, fallback time.Duration) time.Duration {
	t.Helper()
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		t.Fatalf("%s=%q is not a valid duration: %v", key, raw, err)
	}
	return d
}

// loadConfig resolves configuration from the environment, failing loudly on
// anything missing. It deliberately does not t.Skip: the kubeadm_bringup build
// tag is already the opt-in, so reaching this function means the caller asked
// for a real two-VM bring-up and a silent skip would be a lie.
func loadConfig(t *testing.T) config {
	t.Helper()

	c := config{
		nicoVMSBin:        os.Getenv("NKE_BRINGUP_NICO_VMS_BIN"),
		provisionMode:     envOr("NKE_BRINGUP_PROVISION_MODE", provisionCommunity),
		agentDeb:          os.Getenv("NKE_BRINGUP_AGENT_DEB"),
		kubeadmDeb:        os.Getenv("NKE_BRINGUP_KUBEADM_DEB"),
		kubeadmDebVersion: os.Getenv("NKE_BRINGUP_KUBEADM_VERSION"),
		nodeImageURL:      os.Getenv("NKE_BRINGUP_NODE_IMAGE_URL"),
		workDir:           os.Getenv("NKE_BRINGUP_WORK_DIR"),
		bridgeName:        envOr("NKE_BRINGUP_BRIDGE_NAME", os.Getenv("NICO_VMS_BRIDGE_NAME")),
		// The guests sit on nico-vms's bridge, so its CIDR is the one that
		// gives the address they can reach the host on. Read the NICO_VMS_
		// value rather than making the caller repeat it.
		bridgeCIDR: envOr("NKE_BRINGUP_BRIDGE_CIDR",
			envOr("NICO_VMS_BRIDGE_CIDR", defaultBridgeCIDR)),
		aptRepoURL:  envOr("NKE_BRINGUP_APT_REPO_URL", defaultAptRepoURL),
		aptGpgURL:   envOr("NKE_BRINGUP_APT_GPG_URL", defaultAptGpgURL),
		aptSuite:    envOr("NKE_BRINGUP_APT_SUITE", defaultAptSuite),
		aptComp:     envOr("NKE_BRINGUP_APT_COMPONENT", defaultAptComp),
		podCIDR:     envOr("NKE_BRINGUP_POD_CIDR", defaultPodCIDR),
		serviceCIDR: envOr("NKE_BRINGUP_SERVICE_CIDR", defaultServiceCIDR),
		sshPasswordHash: envOr("NKE_BRINGUP_SSH_PASSWORD_HASH",
			"$6$nke-cluster-bringup$KrSrwhFvUJrjMqlVELHsBSx5G1QO8RtNyG8GP6Jrh3.dRgCWQ8GgY79hCsLwlcMz1F0VfeyzL6lJEhwYO5Ay00"),
		initTimeout:   envDuration(t, "NKE_BRINGUP_INIT_TIMEOUT", defaultInitTimeout),
		joinTimeout:   envDuration(t, "NKE_BRINGUP_JOIN_TIMEOUT", defaultJoinTimeout),
		cniTimeout:    envDuration(t, "NKE_BRINGUP_CNI_TIMEOUT", defaultCNITimeout),
		readyTimeout:  envDuration(t, "NKE_BRINGUP_READY_TIMEOUT", defaultReadyTimeout),
		calicoVersion: envOr("NKE_BRINGUP_CALICO_VERSION", cluster.DefaultCalicoVersion),
		keepVMs:       os.Getenv("NKE_BRINGUP_KEEP_VMS") != "",
	}

	// Only the nico-sim binary is required here. The kubeadm settings are
	// validated separately by requireKubeadmConfig, because the smoke test
	// needs none of them: it boots stock Ubuntu and installs nothing, which is
	// exactly what lets it run when the NVKE apt repo is unreachable.
	if c.nicoVMSBin == "" {
		t.Fatal("missing required configuration:\n" +
			"  - NKE_BRINGUP_NICO_VMS_BIN (path to the nico-sim binary; build it with `CGO_ENABLED=1 make -C controller/nico-sim build-privileged`)")
	}

	abs, err := filepath.Abs(c.nicoVMSBin)
	if err != nil {
		t.Fatalf("resolve NKE_BRINGUP_NICO_VMS_BIN %q: %v", c.nicoVMSBin, err)
	}
	c.nicoVMSBin = abs
	if _, err := os.Stat(c.nicoVMSBin); err != nil {
		t.Fatalf("nico-sim binary not usable at %s: %v", c.nicoVMSBin, err)
	}

	// nico-vms resolves its seed relative to its own working directory, so it
	// must be handed an absolute path or it exits 1. The binary lands at
	// <module-root>/bin/nico-sim, which makes the seed derivable; the override
	// exists for anyone running a binary from somewhere else.
	c.seedPath = os.Getenv("NKE_BRINGUP_SEED_PATH")
	if c.seedPath == "" {
		c.seedPath = filepath.Join(
			filepath.Dir(filepath.Dir(c.nicoVMSBin)),
			"internal", "seed", "default-seed.yaml",
		)
	}
	if c.seedPath, err = filepath.Abs(c.seedPath); err != nil {
		t.Fatalf("resolve seed path: %v", err)
	}
	if _, err := os.Stat(c.seedPath); err != nil {
		t.Fatalf("nico-vms seed not readable at %s: %v\n"+
			"set NKE_BRINGUP_SEED_PATH if the binary is not at <module-root>/bin/nico-sim", c.seedPath, err)
	}

	// A `go test` runs with the package directory as its working directory,
	// so the template is simply alongside us under testdata/.
	c.templatePath = envOr("NKE_BRINGUP_CLOUD_INIT_TEMPLATE",
		filepath.Join("testdata", "user-data.yml.tmpl"))
	if c.templatePath, err = filepath.Abs(c.templatePath); err != nil {
		t.Fatalf("resolve cloud-init template path: %v", err)
	}
	if _, err := os.Stat(c.templatePath); err != nil {
		t.Fatalf("cloud-init template not readable at %s: %v", c.templatePath, err)
	}

	c.provisionScript = envOr("NKE_BRINGUP_PROVISION_SCRIPT", defaultProvisionScript)
	if c.provisionScript, err = filepath.Abs(c.provisionScript); err != nil {
		t.Fatalf("resolve provision script path: %v", err)
	}

	return c
}

// requireKubeadmConfig validates the settings only the kubeadm bring-up needs.
// Kept apart from loadConfig so the smoke test can run with neither of them
// set, and so a missing package pin fails before two VMs are booted rather
// than after.
func requireKubeadmConfig(t *testing.T, c config) {
	t.Helper()
	var missing []string
	switch c.provisionMode {
	case provisionCommunity:
		if c.agentDeb == "" {
			missing = append(missing, "NKE_BRINGUP_AGENT_DEB (path to an nvke-agent .deb; build it with `make -C agent deb-amd64`)")
		}
	case provisionApt:
		if c.kubeadmDeb == "" {
			missing = append(missing, "NKE_BRINGUP_KUBEADM_DEB (e.g. nke-kubeadm-distro-1.36.3)")
		}
		if c.kubeadmDebVersion == "" {
			missing = append(missing, "NKE_BRINGUP_KUBEADM_VERSION (pinned apt version, e.g. 6)")
		}
	default:
		t.Fatalf("NKE_BRINGUP_PROVISION_MODE=%q is not one of %q or %q", c.provisionMode, provisionCommunity, provisionApt)
	}
	if len(missing) > 0 {
		t.Fatalf("missing required configuration:\n  - %s", strings.Join(missing, "\n  - "))
	}
}

// resolveWorkDir returns the directory for the nico-vms db, its log, the
// rendered cloud-init bodies and the fetched kubeconfig. A caller-supplied
// directory is kept on exit so CI can upload it; a temporary one is removed
// unless the run asked for the VMs to be kept.
func resolveWorkDir(t *testing.T, c config) string {
	t.Helper()
	if c.workDir != "" {
		if err := os.MkdirAll(c.workDir, 0o755); err != nil {
			t.Fatalf("create work dir %s: %v", c.workDir, err)
		}
		abs, err := filepath.Abs(c.workDir)
		if err != nil {
			t.Fatalf("resolve work dir: %v", err)
		}
		return abs
	}
	dir, err := os.MkdirTemp("", "nvke-kubeadm-bringup-")
	if err != nil {
		t.Fatalf("create temp work dir: %v", err)
	}
	t.Cleanup(func() {
		if c.keepVMs {
			t.Logf("NKE_BRINGUP_KEEP_VMS set — keeping work dir %s", dir)
			return
		}
		_ = os.RemoveAll(dir)
	})
	return dir
}

// agentAddr is the plaintext gRPC endpoint the cloud-init drop-in configures
// nvke-agent to serve on.
func agentAddr(ip string, port int) string { return fmt.Sprintf("%s:%d", ip, port) }
