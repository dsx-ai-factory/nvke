// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package qualification_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

//go:embed probe.py
var smokeProbe string

type imageExpectation struct {
	SchemaVersion string `json:"schema_version"`
	Ubuntu        string `json:"ubuntu"`
	Arch          string `json:"arch"`
	KernelRelease string `json:"kernel_release"`
	BuildID       string `json:"build_id"`
}

type smokeTarget struct {
	Manifest        string           `json:"manifest"`
	Address         string           `json:"address"`
	User            string           `json:"user"`
	SSHConfig       string           `json:"ssh_config"`
	ProvisioningMAC string           `json:"provisioning_mac"`
	NICDriver       string           `json:"nic_driver"`
	RequiredModules []string         `json:"required_modules"`
	GPUCount        int              `json:"gpu_count"`
	Expected        imageExpectation `json:"-"`
}

type smokeConfig struct {
	MachineTypeID string        `json:"machine_type_id"`
	Targets       []smokeTarget `json:"targets"`
}

type smokeInterface struct {
	MAC       string   `json:"mac"`
	Addresses []string `json:"addresses"`
	Carrier   bool     `json:"carrier"`
	Driver    string   `json:"driver"`
}

type smokeFacts struct {
	Ubuntu         string                    `json:"ubuntu"`
	Distro         string                    `json:"distro"`
	Arch           string                    `json:"arch"`
	KernelRelease  string                    `json:"kernel_release"`
	BuildID        string                    `json:"build_id"`
	CloudInit      string                    `json:"cloud_init"`
	RootSource     string                    `json:"root_source"`
	Interfaces     map[string]smokeInterface `json:"interfaces"`
	RouteInterface string                    `json:"route_interface"`
	Modules        []string                  `json:"modules"`
	GPUCount       int                       `json:"gpu_count"`
}

func readStrictJSON(path string, dst any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	return decodeStrictJSON(raw, dst)
}

func decodeStrictJSON(raw []byte, dst any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("expected exactly one JSON object")
	}
	return nil
}

var safeIdentifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

func loadSmokeConfig(path, requestedType string) (smokeConfig, error) {
	var cfg smokeConfig
	if err := readStrictJSON(path, &cfg); err != nil {
		return cfg, err
	}
	if cfg.MachineTypeID == "" || (requestedType != "" && requestedType != cfg.MachineTypeID) {
		return cfg, errors.New("machine_type_id is missing or does not match DSX_MACHINE_TYPE_ID")
	}
	if len(cfg.Targets) != 2 {
		return cfg, errors.New("need exactly one target for Ubuntu 24.04 and one for 26.04")
	}
	seen := make(map[string]bool)
	addresses := make(map[string]bool)
	for i := range cfg.Targets {
		target := &cfg.Targets[i]
		if net.ParseIP(target.Address) == nil || !safeIdentifier.MatchString(target.User) || target.SSHConfig == "" {
			return cfg, fmt.Errorf("target %d needs an IP address, SSH user, and SSH config", i)
		}
		if addresses[target.Address] {
			return cfg, errors.New("targets must refer to distinct provisioned nodes")
		}
		addresses[target.Address] = true
		mac, err := net.ParseMAC(target.ProvisioningMAC)
		if err != nil || len(mac) != 6 || target.NICDriver == "" || len(target.RequiredModules) == 0 || target.GPUCount < 0 {
			return cfg, fmt.Errorf("target %d needs a MAC, NIC driver, required modules, and nonnegative GPU count", i)
		}
		target.ProvisioningMAC = mac.String()
		if target.Manifest == "" {
			return cfg, fmt.Errorf("target %d has no build manifest", i)
		}
		if !filepath.IsAbs(target.Manifest) {
			target.Manifest = filepath.Join(filepath.Dir(path), target.Manifest)
		}
		if !filepath.IsAbs(target.SSHConfig) {
			target.SSHConfig = filepath.Join(filepath.Dir(path), target.SSHConfig)
		}
		if _, err := os.Stat(target.SSHConfig); err != nil {
			return cfg, fmt.Errorf("SSH config: %w", err)
		}
		if err := readStrictJSON(target.Manifest, &target.Expected); err != nil {
			return cfg, err
		}
		e := target.Expected
		if e.SchemaVersion != "nvke.os-image.identity/v1" || (e.Ubuntu != "24.04" && e.Ubuntu != "26.04") ||
			(e.Arch != "amd64" && e.Arch != "arm64") || e.KernelRelease == "" || !safeIdentifier.MatchString(e.BuildID) || seen[e.Ubuntu] {
			return cfg, fmt.Errorf("target %d has an invalid or duplicate build identity", i)
		}
		seen[e.Ubuntu] = true
	}
	return cfg, nil
}

func probeSSH(ctx context.Context, target smokeTarget) (smokeFacts, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", "-F", target.SSHConfig,
		"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=15",
		"-l", target.User, "--", target.Address, "python3 -")
	cmd.Stdin = strings.NewReader(smokeProbe)
	raw, err := cmd.Output()
	if err != nil {
		// Remote errors can contain sensitive data. Report the command failure
		// without printing arbitrary stdout or stderr.
		return smokeFacts{}, fmt.Errorf("probe host %s: %w", target.Address, err)
	}
	var facts smokeFacts
	if err := decodeStrictJSON(raw, &facts); err != nil {
		return facts, fmt.Errorf("host probe response: %w", err)
	}
	return facts, nil
}

func checkSmoke(target smokeTarget, facts smokeFacts) error {
	var failures []error
	expect := func(name, got, want string) {
		if got != want {
			failures = append(failures, fmt.Errorf("%s: got %q, want %q", name, got, want))
		}
	}
	expect("distro", facts.Distro, "ubuntu")
	expect("Ubuntu", facts.Ubuntu, target.Expected.Ubuntu)
	expect("architecture", facts.Arch, target.Expected.Arch)
	expect("kernel", facts.KernelRelease, target.Expected.KernelRelease)
	expect("build ID", facts.BuildID, target.Expected.BuildID)
	expect("cloud-init", facts.CloudInit, "done")
	if !strings.HasPrefix(facts.RootSource, "/dev/") {
		failures = append(failures, errors.New("root filesystem is not backed by an expected block device"))
	}
	iface, found := facts.Interfaces[facts.RouteInterface]
	if !found || !iface.Carrier {
		failures = append(failures, errors.New("route to SSH client has no interface with carrier"))
	} else {
		expect("provisioning NIC MAC", strings.ToLower(iface.MAC), target.ProvisioningMAC)
		expect("provisioning NIC driver", iface.Driver, target.NICDriver)
		hasAddress := false
		for _, address := range iface.Addresses {
			if net.ParseIP(address).Equal(net.ParseIP(target.Address)) {
				hasAddress = true
			}
		}
		if !hasAddress {
			failures = append(failures, errors.New("routed provisioning NIC does not own the allocated address"))
		}
	}
	modules := make(map[string]bool)
	for _, module := range facts.Modules {
		modules[module] = true
	}
	for _, module := range target.RequiredModules {
		if !modules[module] {
			failures = append(failures, fmt.Errorf("required driver module %q is absent", module))
		}
	}
	if target.GPUCount > 0 && facts.GPUCount != target.GPUCount {
		failures = append(failures, fmt.Errorf("healthy GPUs: got %d, want %d", facts.GPUCount, target.GPUCount))
	}
	return errors.Join(failures...)
}
