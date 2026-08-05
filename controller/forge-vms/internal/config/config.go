// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

// Package config contains forge-vms runtime configuration parsing and validation.
package config

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

func envOrDefault(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

func envOrDefaultInt(key string, defaultVal int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("failed to parse %s=%q as int: %w", key, v, err)
	}
	return n, nil
}

func envOrDefaultDuration(key, defaultVal string) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		v = defaultVal
	}
	return time.ParseDuration(v)
}

type Config struct {
	ListenAddr             string
	DBPath                 string
	VMBackend              string
	LibvirtURI             string
	QEMUSystemPath         string
	BridgeName             string
	BridgeCIDR             string
	BaseImage              string
	ReadinessProbeInterval time.Duration
	ReadinessTimeout       time.Duration
	QEMUImgPath            string
	CloudLocalDSPath       string
	SeedPath               string
	VMVCPUs                int
	VMMemoryMiB            int
	VMDiskGiB              int
	ReconcileWorkers       int
	ReconcileInterval      time.Duration
	LogRequestMethods      []string
	DHCPHostsDir           string
	DHCPLeaseFile          string
	LogFormat              string
	ConsoleSSHPort         int
	CleanupOrphanedVMs     bool
	BGPEnabled             bool
	BGPListenPort          int
	BGPLocalASN            uint32
	BGPPeerASN             uint32
	BGPHoldTime            int
	BGPKeepalive           int
	BGPAllowedVIPCIDRs     []string
	PromtailTargetsFile    string
	PromtailLogPathPrefix  string
	DiagSSHEnabled         bool
	DiagSSHUser            string
	DiagSSHPassword        string
}

func Parse(logger *slog.Logger) (Config, error) {
	readinessProbeInterval, err := envOrDefaultDuration("FORGE_VMS_READINESS_PROBE_INTERVAL", "3s")
	if err != nil {
		return Config{}, fmt.Errorf("failed to parse FORGE_VMS_READINESS_PROBE_INTERVAL: %w", err)
	}

	readinessTimeout, err := envOrDefaultDuration("FORGE_VMS_READINESS_TIMEOUT", "15m")
	if err != nil {
		return Config{}, fmt.Errorf("failed to parse FORGE_VMS_READINESS_TIMEOUT: %w", err)
	}

	reconcileInterval, err := envOrDefaultDuration("FORGE_VMS_RECONCILE_INTERVAL", "5s")
	if err != nil {
		return Config{}, fmt.Errorf("failed to parse FORGE_VMS_RECONCILE_INTERVAL: %w", err)
	}

	vmVCPUs, err := envOrDefaultInt("FORGE_VMS_VM_VCPUS", 2)
	if err != nil {
		return Config{}, err
	}
	vmMemoryMiB, err := envOrDefaultInt("FORGE_VMS_VM_MEMORY_MIB", 2048)
	if err != nil {
		return Config{}, err
	}
	vmDiskGiB, err := envOrDefaultInt("FORGE_VMS_VM_DISK_GIB", 20)
	if err != nil {
		return Config{}, err
	}
	reconcileWorkers, err := envOrDefaultInt("FORGE_VMS_RECONCILE_WORKERS", 4)
	if err != nil {
		return Config{}, err
	}

	consoleSSHPort, err := envOrDefaultInt("FORGE_VMS_CONSOLE_SSH_PORT", 2222)
	if err != nil {
		return Config{}, err
	}

	bgpEnabled := strings.ToLower(envOrDefault("FORGE_VMS_BGP_ENABLED", "true")) == "true"
	bgpListenPort, err := envOrDefaultInt("FORGE_VMS_BGP_LISTEN_PORT", 10179)
	if err != nil {
		return Config{}, err
	}
	bgpLocalASN, err := envOrDefaultInt("FORGE_VMS_BGP_LOCAL_ASN", 4294967000)
	if err != nil {
		return Config{}, err
	}
	bgpPeerASN, err := envOrDefaultInt("FORGE_VMS_BGP_PEER_ASN", 65100)
	if err != nil {
		return Config{}, err
	}
	bgpHoldTime, err := envOrDefaultInt("FORGE_VMS_BGP_HOLD_TIME", 9)
	if err != nil {
		return Config{}, err
	}
	bgpKeepalive, err := envOrDefaultInt("FORGE_VMS_BGP_KEEPALIVE", 3)
	if err != nil {
		return Config{}, err
	}
	bgpVIPCIDRs := parseCommaSeparated(os.Getenv("FORGE_VMS_BGP_VIP_CIDRS"))

	cfg := Config{
		ListenAddr:             envOrDefault("FORGE_VMS_LISTEN_ADDR", ":18080"),
		DBPath:                 envOrDefault("FORGE_VMS_DB_PATH", "./var/forge-vms.db"),
		VMBackend:              envOrDefault("FORGE_VMS_VM_BACKEND", "auto"),
		LibvirtURI:             envOrDefault("FORGE_VMS_LIBVIRT_URI", "qemu:///session"),
		QEMUSystemPath:         os.Getenv("FORGE_VMS_QEMU_SYSTEM_PATH"),
		BridgeName:             envOrDefault("FORGE_VMS_BRIDGE_NAME", "cwebr0"),
		BridgeCIDR:             os.Getenv("FORGE_VMS_BRIDGE_CIDR"),
		BaseImage:              envOrDefault("FORGE_VMS_BASE_IMAGE", "images/ubuntu-cloud.qcow2"),
		ReadinessProbeInterval: readinessProbeInterval,
		ReadinessTimeout:       readinessTimeout,
		QEMUImgPath:            envOrDefault("FORGE_VMS_QEMU_IMG_PATH", "qemu-img"),
		CloudLocalDSPath:       envOrDefault("FORGE_VMS_CLOUD_LOCALDS_PATH", "cloud-localds"),
		SeedPath:               envOrDefault("FORGE_VMS_SEED_PATH", "./internal/seed/default-seed.yaml"),
		VMVCPUs:                vmVCPUs,
		VMMemoryMiB:            vmMemoryMiB,
		VMDiskGiB:              vmDiskGiB,
		ReconcileWorkers:       reconcileWorkers,
		ReconcileInterval:      reconcileInterval,
		LogRequestMethods:      parseLogRequestMethods(envOrDefault("FORGE_VMS_LOG_REQUEST_METHODS", LogPresetWrite)),
		DHCPHostsDir:           envOrDefault("FORGE_VMS_DHCP_HOSTS_DIR", "/var/lib/forge-vms/dhcp-hosts"),
		DHCPLeaseFile:          envOrDefault("FORGE_VMS_DHCP_LEASE_FILE", "/var/run/forge-vms-dnsmasq.leases"),
		LogFormat:              ParseLogFormat(),
		ConsoleSSHPort:         consoleSSHPort,
		CleanupOrphanedVMs:     strings.ToLower(envOrDefault("FORGE_VMS_CLEANUP_ORPHANED_VMS", "false")) == "true",
		BGPEnabled:             bgpEnabled,
		BGPListenPort:          bgpListenPort,
		BGPLocalASN:            uint32(bgpLocalASN),
		BGPPeerASN:             uint32(bgpPeerASN),
		BGPHoldTime:            bgpHoldTime,
		BGPKeepalive:           bgpKeepalive,
		BGPAllowedVIPCIDRs:     bgpVIPCIDRs,
		PromtailTargetsFile:    os.Getenv("FORGE_VMS_PROMTAIL_TARGETS_FILE"),
		PromtailLogPathPrefix:  os.Getenv("FORGE_VMS_PROMTAIL_LOG_PATH_PREFIX"),
		DiagSSHEnabled:         strings.ToLower(envOrDefault("FORGE_VMS_DIAG_SSH_ENABLED", "true")) == "true",
		DiagSSHUser:            envOrDefault("FORGE_VMS_DIAG_SSH_USER", "forge-diag"),
		DiagSSHPassword:        envOrDefault("FORGE_VMS_DIAG_SSH_PASSWORD", "forge-diag"),
	}

	if err := cfg.normalize(logger); err != nil {
		return Config{}, err
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func (c Config) Validate() error {
	if c.ListenAddr == "" {
		return fmt.Errorf("FORGE_VMS_LISTEN_ADDR is required")
	}
	if c.DBPath == "" {
		return fmt.Errorf("FORGE_VMS_DB_PATH is required")
	}
	if c.BridgeName == "" {
		return fmt.Errorf("FORGE_VMS_BRIDGE_NAME is required")
	}
	if _, _, err := net.ParseCIDR(c.BridgeCIDR); err != nil {
		return fmt.Errorf("FORGE_VMS_BRIDGE_CIDR must be a valid CIDR: %w", err)
	}
	backend := c.EffectiveVMBackend()
	switch backend {
	case VMBackendMock, VMBackendLibvirt, VMBackendDarwinQEMU:
		// valid
	default:
		return fmt.Errorf("FORGE_VMS_VM_BACKEND must be one of %q, %q, %q, %q",
			VMBackendAuto, VMBackendMock, VMBackendLibvirt, VMBackendDarwinQEMU)
	}
	if backend != VMBackendMock && c.BaseImage == "" {
		return fmt.Errorf("FORGE_VMS_BASE_IMAGE is required when effective VM backend is not %q", VMBackendMock)
	}
	if backend == VMBackendDarwinQEMU && runtime.GOOS != "darwin" {
		return fmt.Errorf("FORGE_VMS_VM_BACKEND=%q is only supported on darwin hosts", VMBackendDarwinQEMU)
	}
	if backend == VMBackendLibvirt && strings.TrimSpace(c.LibvirtURI) == "" {
		return fmt.Errorf("FORGE_VMS_LIBVIRT_URI is required for %q backend", VMBackendLibvirt)
	}
	if backend == VMBackendDarwinQEMU && strings.TrimSpace(c.EffectiveQEMUSystemPath()) == "" {
		return fmt.Errorf("FORGE_VMS_QEMU_SYSTEM_PATH is required for %q backend", VMBackendDarwinQEMU)
	}
	if c.ReadinessProbeInterval <= 0 {
		return fmt.Errorf("FORGE_VMS_READINESS_PROBE_INTERVAL must be > 0")
	}
	if c.ReadinessTimeout <= 0 {
		return fmt.Errorf("FORGE_VMS_READINESS_TIMEOUT must be > 0")
	}
	if c.VMVCPUs <= 0 || c.VMMemoryMiB <= 0 || c.VMDiskGiB <= 0 {
		return fmt.Errorf("FORGE_VMS_VM_* values must be > 0")
	}
	if c.ReconcileWorkers <= 0 {
		return fmt.Errorf("FORGE_VMS_RECONCILE_WORKERS must be > 0")
	}
	if c.ReconcileInterval <= 0 {
		return fmt.Errorf("FORGE_VMS_RECONCILE_INTERVAL must be > 0")
	}
	switch c.LogFormat {
	case LogFormatJSON, LogFormatText:
		// valid
	default:
		return fmt.Errorf("FORGE_VMS_LOG_FORMAT must be %q or %q (got %q)", LogFormatJSON, LogFormatText, c.LogFormat)
	}

	return nil
}

func (c Config) hasMockLibvirtURI() bool {
	return strings.HasPrefix(strings.ToLower(c.LibvirtURI), "mock://")
}

const (
	VMBackendAuto       = "auto"
	VMBackendMock       = "mock"
	VMBackendLibvirt    = "libvirt"
	VMBackendDarwinQEMU = "darwin-qemu"
)

func (c Config) EffectiveVMBackend() string {
	switch strings.ToLower(strings.TrimSpace(c.VMBackend)) {
	case "", VMBackendAuto:
		if c.hasMockLibvirtURI() {
			return VMBackendMock
		}
		if runtime.GOOS == "darwin" {
			return VMBackendDarwinQEMU
		}
		return VMBackendLibvirt
	case VMBackendMock:
		return VMBackendMock
	case VMBackendLibvirt:
		return VMBackendLibvirt
	case VMBackendDarwinQEMU:
		return VMBackendDarwinQEMU
	default:
		return VMBackendLibvirt
	}
}

func (c Config) EffectiveQEMUSystemPath() string {
	if strings.TrimSpace(c.QEMUSystemPath) != "" {
		return c.QEMUSystemPath
	}
	if runtime.GOARCH == "arm64" {
		return "qemu-system-aarch64"
	}
	return "qemu-system-x86_64"
}

func (c Config) WorkingDirectory() string {
	return filepath.Join(filepath.Dir(c.DBPath), "instances")
}

// FilesDirectory returns the directory from which the file server serves
// files. Uses FORGE_VMS_FILES_DIR if set, otherwise defaults to "files/"
// alongside the database.
func (c Config) FilesDirectory() string {
	if dir := os.Getenv("FORGE_VMS_FILES_DIR"); dir != "" {
		return dir
	}
	return filepath.Join(filepath.Dir(c.DBPath), "files")
}

// ImageCacheDirectory returns the directory used to cache downloaded qcow2 images
// and kernel files. Uses FORGE_VMS_IMAGE_CACHE_DIR if set, otherwise falls back
// to a stable location under the user's home directory so caches survive test runs.
func (c Config) ImageCacheDirectory() string {
	if dir := os.Getenv("FORGE_VMS_IMAGE_CACHE_DIR"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err == nil {
		return filepath.Join(home, ".cache", "forge-vms", "images")
	}
	// Fallback to DB-relative path
	return filepath.Join(filepath.Dir(c.DBPath), "cache", "images")
}

const (
	LogFormatJSON = "json"
	LogFormatText = "text"
)

// ParseLogFormat reads FORGE_VMS_LOG_FORMAT and returns the normalised value.
// Safe to call before full config parsing — only reads one env var.
func ParseLogFormat() string {
	return envOrDefault("FORGE_VMS_LOG_FORMAT", LogFormatJSON)
}

const (
	LogPresetWrite = "write"
	LogPresetAll   = "all"
	LogPresetNone  = "none"
)

var writeMethods = []string{"POST", "PUT", "PATCH", "DELETE"}

var allMethods = []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"}

// parseLogRequestMethods splits a raw env-var value into a normalised slice.
// The presets "write" and "all" are expanded; "none" yields a nil slice.
func parseLogRequestMethods(raw string) []string {
	v := strings.ToLower(strings.TrimSpace(raw))
	switch v {
	case "", LogPresetWrite:
		return writeMethods
	case LogPresetAll:
		return allMethods
	case LogPresetNone:
		return nil
	default:
		var methods []string
		for m := range strings.SplitSeq(v, ",") {
			if s := strings.ToUpper(strings.TrimSpace(m)); s != "" {
				methods = append(methods, s)
			}
		}
		return methods
	}
}

func parseCommaSeparated(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var result []string
	for v := range strings.SplitSeq(s, ",") {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

func (c *Config) normalize(logger *slog.Logger) error {
	// DBPath must be absolute — libvirt domain XML requires absolute file paths
	// for disk overlays and serial log files derived from WorkingDirectory().
	if c.DBPath != "" {
		absDB, err := normalizePath(c.DBPath)
		if err != nil {
			return fmt.Errorf("failed to normalize FORGE_VMS_DB_PATH: %w", err)
		}
		c.DBPath = absDB
	}

	if strings.TrimSpace(c.BaseImage) != "" {
		absBaseImage, err := normalizePath(c.BaseImage)
		if err != nil {
			return fmt.Errorf("failed to normalize FORGE_VMS_BASE_IMAGE: %w", err)
		}
		c.BaseImage = absBaseImage
	}

	if c.EffectiveVMBackend() == VMBackendDarwinQEMU {
		if err := c.normalizeDarwinVMNetBridgeCIDR(logger); err != nil {
			return err
		}
	} else {
		if c.BridgeCIDR == "" {
			logger.Info("No bridge cidr set, using default", "bridgeCIDR", "172.30.0.0/24")
			c.BridgeCIDR = "172.30.0.0/24"
		}
	}

	return nil
}

func normalizePath(path string) (string, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return "", nil
	}

	switch {
	case trimmed == "~":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		trimmed = home
	case strings.HasPrefix(trimmed, "~/"):
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		trimmed = filepath.Join(home, strings.TrimPrefix(trimmed, "~/"))
	}

	return filepath.Abs(trimmed)
}

func (c *Config) normalizeDarwinVMNetBridgeCIDR(logger *slog.Logger) error {
	if runtime.GOOS != "darwin" || c.EffectiveVMBackend() != VMBackendDarwinQEMU {
		return nil
	}
	if c.BridgeCIDR != "" {
		return nil
	}

	helperPath := findVMNetHelper()
	if helperPath == "" {
		return fmt.Errorf(
			"vmnet-helper not found; set FORGE_VMS_BRIDGE_CIDR manually or install vmnet-helper " +
				"(brew tap nirs/vmnet-helper && brew install vmnet-helper)",
		)
	}

	info, err := probeVMNetSharedSubnet(context.Background(), helperPath)
	if err != nil {
		return fmt.Errorf("failed to probe vmnet shared subnet: %w", err)
	}
	cidr, err := vmnetProbeToCIDR(info)
	if err != nil {
		return fmt.Errorf("failed to derive CIDR from vmnet probe: %w", err)
	}

	logger.Info("Detected vmnet shared subnet via probe",
		"cidr", cidr,
		"gateway", info.StartAddress,
		"endAddress", info.EndAddress,
	)
	c.BridgeCIDR = cidr
	return nil
}

type vmnetProbeResult struct {
	StartAddress string `json:"vmnet_start_address"`
	EndAddress   string `json:"vmnet_end_address"`
	SubnetMask   string `json:"vmnet_subnet_mask"`
	InterfaceID  string `json:"vmnet_interface_id"`
	MACAddress   string `json:"vmnet_mac_address"`
}

// probeVMNetSharedSubnet launches vmnet-helper briefly to discover the
// shared-mode network parameters. vmnet-helper writes a JSON line to stdout
// after creating the interface, then exits when the dummy fd fails.
func probeVMNetSharedSubnet(ctx context.Context, vmnetHelperPath string) (vmnetProbeResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, vmnetHelperPath, "--operation-mode=shared", "--fd=2")
	out, err := cmd.Output()
	if len(out) == 0 {
		return vmnetProbeResult{}, fmt.Errorf("vmnet-helper probe produced no output: %w", err)
	}

	var info vmnetProbeResult
	if err := json.Unmarshal(out, &info); err != nil {
		return vmnetProbeResult{}, fmt.Errorf("failed to parse vmnet-helper probe output: %w", err)
	}
	return info, nil
}

func vmnetProbeToCIDR(info vmnetProbeResult) (string, error) {
	gateway := net.ParseIP(info.StartAddress).To4()
	if gateway == nil {
		return "", fmt.Errorf("invalid vmnet start address %q", info.StartAddress)
	}
	mask := net.ParseIP(info.SubnetMask).To4()
	if mask == nil {
		return "", fmt.Errorf("invalid vmnet subnet mask %q", info.SubnetMask)
	}
	network := make(net.IP, net.IPv4len)
	for i := 0; i < net.IPv4len; i++ {
		network[i] = gateway[i] & mask[i]
	}
	prefixLen, _ := net.IPMask(mask).Size()
	return fmt.Sprintf("%s/%d", network, prefixLen), nil
}

func findVMNetHelper() string {
	candidates := []string{
		"/opt/vmnet-helper/bin/vmnet-helper",
		"/opt/homebrew/opt/vmnet-helper/libexec/vmnet-helper",
		"/usr/local/opt/vmnet-helper/libexec/vmnet-helper",
	}
	for _, p := range candidates {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	return ""
}
