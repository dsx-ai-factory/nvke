// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

// Package artifacts builds VM disk and cloud-init artifacts.
package artifacts

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Builder struct {
	QEMUImgPath      string
	CloudLocalDSPath string
	BaseImage        string
	MockMode         bool
}

type BuildInput struct {
	WorkingDir   string
	InstanceID   string
	Hostname     string
	UserData     string
	NICMAC       string
	IPAddress    string
	GatewayIP    string
	PrefixLength int
	BaseImage    string // per-instance override; if empty, uses Builder.BaseImage
	DiskGiB      int    // required: disk size in GiB for the instance
}

type BuildOutput struct {
	DiskPath    string
	SeedISOPath string
}

func (b Builder) Build(ctx context.Context, in BuildInput) (BuildOutput, error) {
	if err := os.MkdirAll(in.WorkingDir, 0o755); err != nil {
		return BuildOutput{}, fmt.Errorf("failed to create working directory: %w", err)
	}

	diskPath := filepath.Join(in.WorkingDir, "disk.qcow2")
	seedISOPath := filepath.Join(in.WorkingDir, "seed.iso")
	userDataPath := filepath.Join(in.WorkingDir, "user-data")
	metaDataPath := filepath.Join(in.WorkingDir, "meta-data")
	networkConfigPath := filepath.Join(in.WorkingDir, "network-config")

	if in.UserData == "" {
		in.UserData = "#cloud-config\n"
	}
	if err := os.WriteFile(userDataPath, []byte(in.UserData), 0o644); err != nil {
		return BuildOutput{}, fmt.Errorf("failed to write user-data: %w", err)
	}
	metaData := fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\nasn: 4294967000\n", in.InstanceID, in.Hostname)
	if err := os.WriteFile(metaDataPath, []byte(metaData), 0o644); err != nil {
		return BuildOutput{}, fmt.Errorf("failed to write meta-data: %w", err)
	}
	var networkConfig string
	if in.IPAddress == "" {
		// DHCP mode: VM gets its IP from dnsmasq.
		networkConfig = fmt.Sprintf(`version: 2
ethernets:
  eth0:
    match:
      macaddress: "%s"
    set-name: eth0
    dhcp4: true
`, in.NICMAC)
	} else {
		// Static mode: IP baked into seed ISO from IPAM allocation.
		// DNS points to the bridge gateway where dnsmasq listens.
		networkConfig = fmt.Sprintf(`version: 2
ethernets:
  eth0:
    match:
      macaddress: "%s"
    set-name: eth0
    addresses:
      - %s/%d
    nameservers:
      addresses:
        - %s
    routes:
      - to: default
        via: %s
`, in.NICMAC, in.IPAddress, in.PrefixLength, in.GatewayIP, in.GatewayIP)
	}
	if err := os.WriteFile(networkConfigPath, []byte(networkConfig), 0o644); err != nil {
		return BuildOutput{}, fmt.Errorf("failed to write network-config: %w", err)
	}

	if b.MockMode {
		if err := os.WriteFile(diskPath, []byte("mock-disk"), 0o644); err != nil {
			return BuildOutput{}, fmt.Errorf("failed to write mock disk: %w", err)
		}
		if err := os.WriteFile(seedISOPath, []byte("mock-seed"), 0o644); err != nil {
			return BuildOutput{}, fmt.Errorf("failed to write mock seed: %w", err)
		}
		return BuildOutput{
			DiskPath:    diskPath,
			SeedISOPath: seedISOPath,
		}, nil
	}

	baseImage := b.BaseImage
	if in.BaseImage != "" {
		baseImage = in.BaseImage
	}
	if _, err := os.Stat(baseImage); err != nil {
		return BuildOutput{}, fmt.Errorf("base image %q does not exist: %w", baseImage, err)
	}
	diskGiB := in.DiskGiB
	qemuArgs := []string{
		"create",
		"-f", "qcow2",
		"-F", "qcow2",
		"-b", baseImage,
		diskPath,
		fmt.Sprintf("%dG", diskGiB),
	}
	if out, err := exec.CommandContext(ctx, b.QEMUImgPath, qemuArgs...).CombinedOutput(); err != nil {
		return BuildOutput{}, fmt.Errorf("qemu-img create failed: %w: %s", err, string(out))
	}

	cloudLocalDSArgs := []string{
		"-N", networkConfigPath,
		seedISOPath,
		userDataPath,
		metaDataPath,
	}
	if out, err := exec.CommandContext(ctx, b.CloudLocalDSPath, cloudLocalDSArgs...).CombinedOutput(); err != nil {
		// On macOS, fallback scripts may fail due to getopt incompatibilities.
		// Build the NoCloud ISO directly with mkisofs as a robust fallback.
		mkisofsErr := buildSeedISOWithMkisofs(ctx, in.WorkingDir, seedISOPath)
		if mkisofsErr != nil {
			return BuildOutput{}, fmt.Errorf(
				"cloud-localds failed: %w: %s; mkisofs fallback failed: %v",
				err,
				strings.TrimSpace(string(out)),
				mkisofsErr,
			)
		}
	}

	return BuildOutput{
		DiskPath:    diskPath,
		SeedISOPath: seedISOPath,
	}, nil
}

func buildSeedISOWithMkisofs(ctx context.Context, workingDir, seedISOPath string) error {
	absWorkingDir, err := filepath.Abs(workingDir)
	if err != nil {
		return fmt.Errorf("failed to resolve working directory %q: %w", workingDir, err)
	}
	absSeedISOPath, err := filepath.Abs(seedISOPath)
	if err != nil {
		return fmt.Errorf("failed to resolve seed iso path %q: %w", seedISOPath, err)
	}

	cmd := exec.CommandContext(
		ctx,
		"mkisofs",
		"-output", absSeedISOPath,
		"-volid", "cidata",
		"-joliet",
		"-rock",
		filepath.Join(absWorkingDir, "user-data"),
		filepath.Join(absWorkingDir, "meta-data"),
		filepath.Join(absWorkingDir, "network-config"),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mkisofs failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
