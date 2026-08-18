// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

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
	var isoToolOut string
	if out, err := exec.CommandContext(ctx, b.CloudLocalDSPath, cloudLocalDSArgs...).CombinedOutput(); err != nil {
		isoToolOut = fmt.Sprintf("cloud-localds(%s) failed: %v; output: %s", b.CloudLocalDSPath, err, strings.TrimSpace(string(out)))
	} else {
		isoToolOut = fmt.Sprintf("cloud-localds(%s) succeeded; output: %q", b.CloudLocalDSPath, strings.TrimSpace(string(out)))
	}

	// Fall back to a direct ISO build whenever seed.iso is missing — whether
	// cloud-localds errored (e.g. getopt incompatibilities on macOS) or exited 0
	// without writing the file. The fallback picks whichever ISO tool is present.
	if _, statErr := os.Stat(seedISOPath); statErr != nil {
		if fbErr := buildSeedISO(ctx, in.WorkingDir, seedISOPath); fbErr != nil {
			return BuildOutput{}, fmt.Errorf(
				"cloud-localds did not produce %s (%s); ISO fallback failed: %v; tools: [%s]",
				seedISOPath, isoToolOut, fbErr, isoToolReport(b.CloudLocalDSPath),
			)
		}
		isoToolOut += "; ISO fallback succeeded"
	}

	// Final check — the ISO must exist now (defends against a fallback that
	// reports success but writes nothing).
	if _, err := os.Stat(seedISOPath); err != nil {
		return BuildOutput{}, fmt.Errorf(
			"seed ISO not found at %s after build (iso-tool: %s; tools: [%s]): %w",
			seedISOPath, isoToolOut, isoToolReport(b.CloudLocalDSPath), err,
		)
	}

	return BuildOutput{
		DiskPath:    diskPath,
		SeedISOPath: seedISOPath,
	}, nil
}

// isoToolReport returns a one-line summary of which ISO-building tools are
// resolvable in PATH. Included in error messages to aid diagnosis on
// environments where cloud-localds or mkisofs may not be installed.
func isoToolReport(cloudLocalDSPath string) string {
	resolve := func(name string) string {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
		return "missing"
	}
	return fmt.Sprintf("cloud-localds(%s)=%s mkisofs=%s genisoimage=%s",
		cloudLocalDSPath,
		resolve(cloudLocalDSPath),
		resolve("mkisofs"),
		resolve("genisoimage"),
	)
}

// buildSeedISO builds the NoCloud seed ISO directly, using whichever ISO tool is
// available — mkisofs or genisoimage (a drop-in fork sharing the same flags).
// Used as a fallback when cloud-localds is unavailable or produces no output.
// The tool preference matches the detection reported by isoToolReport.
func buildSeedISO(ctx context.Context, workingDir, seedISOPath string) error {
	isoTool := ""
	for _, t := range []string{"mkisofs", "genisoimage"} {
		if _, err := exec.LookPath(t); err == nil {
			isoTool = t
			break
		}
	}
	if isoTool == "" {
		return fmt.Errorf("no ISO build tool available (tried mkisofs, genisoimage)")
	}

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
		isoTool,
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
		return fmt.Errorf("%s failed: %w: %s", isoTool, err, strings.TrimSpace(string(out)))
	}
	return nil
}
