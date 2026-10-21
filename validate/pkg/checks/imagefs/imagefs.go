// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package imagefs implements the guestfs-tier checks: facts read from inside
// the image through checks.ImageFS. Each implementation asserts ship state —
// what the artifact holds before any node boots it.
package imagefs

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/dsx-ai-factory/nvke/validate/pkg/checks"
)

// Impls returns the guestfs-tier implementations keyed by check ID.
func Impls() map[string]checks.Func {
	return map[string]checks.Func{
		"INT-004":  checkDpkgMatchesSBOM,
		"HARD-001": maskedUnitsCheck("apt-daily.timer", "apt-daily-upgrade.timer", "unattended-upgrades.service"),
		"HARD-002": maskedUnitsCheck("multipathd.service", "multipathd.socket"),
		"HARD-003": checkEtcdDir,
		"HARD-004": checkKernelFlavor,
		"SPEC-004": checkCalicoManifests,
		"FDE-001":  checkLUKSPresent,
	}
}

// Package is one installed deb.
type Package struct {
	Version      string
	Architecture string
}

// ParseDpkgStatus reads /var/lib/dpkg/status content into the installed
// package set. Only packages in state "install ok installed" count: a
// removed-but-not-purged package is not on the node's attack surface.
func ParseDpkgStatus(status string) (map[string]Package, error) {
	pkgs := make(map[string]Package)
	for _, para := range strings.Split(status, "\n\n") {
		var name, version, arch, state string
		for _, line := range strings.Split(para, "\n") {
			key, value, ok := strings.Cut(line, ":")
			if !ok || strings.HasPrefix(line, " ") {
				continue
			}
			v := strings.TrimSpace(value)
			switch key {
			case "Package":
				name = v
			case "Version":
				version = v
			case "Architecture":
				arch = v
			case "Status":
				state = v
			}
		}
		if name == "" {
			continue
		}
		if state != "install ok installed" {
			continue
		}
		if version == "" {
			return nil, fmt.Errorf("dpkg status: installed package %s has no version", name)
		}
		pkgs[name] = Package{Version: version, Architecture: arch}
	}
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("dpkg status: no installed packages parsed")
	}
	return pkgs, nil
}

func installedPackages(fs checks.ImageFS) (map[string]Package, error) {
	status, err := fs.Cat("/var/lib/dpkg/status")
	if err != nil {
		return nil, fmt.Errorf("reading dpkg status: %w", err)
	}
	return ParseDpkgStatus(status)
}

// checkDpkgMatchesSBOM (INT-004): the image's installed package set equals the
// release SBOM's deb package set, name and version. Severity is decided by the
// registry (warn in v0, pending calibration against real syft output).
func checkDpkgMatchesSBOM(_ context.Context, _ checks.Target, in checks.Inputs) (checks.Outcome, error) {
	if in.SBOMPackages == nil {
		return checks.Outcome{
			Status:   checks.StatusSkip,
			Reason:   checks.SkipInputMissing,
			Evidence: "no SBOM available for this artifact in the bundle",
		}, nil
	}
	installed, err := installedPackages(in.FS)
	if err != nil {
		return checks.Outcome{}, err
	}
	var drift []string
	for name, pkg := range installed {
		sbomVersion, ok := in.SBOMPackages[name]
		switch {
		case !ok:
			drift = append(drift, fmt.Sprintf("+%s=%s (installed, not in SBOM)", name, pkg.Version))
		case sbomVersion != pkg.Version:
			drift = append(drift, fmt.Sprintf("~%s installed=%s sbom=%s", name, pkg.Version, sbomVersion))
		}
	}
	for name, version := range in.SBOMPackages {
		if _, ok := installed[name]; !ok {
			drift = append(drift, fmt.Sprintf("-%s=%s (in SBOM, not installed)", name, version))
		}
	}
	if len(drift) > 0 {
		sort.Strings(drift)
		const show = 5
		shown := drift
		if len(shown) > show {
			shown = shown[:show]
		}
		return checks.Outcome{
			Status:   checks.StatusFail,
			Evidence: fmt.Sprintf("%d package drift(s) vs SBOM: %s", len(drift), strings.Join(shown, "; ")),
		}, nil
	}
	return checks.Outcome{
		Status:   checks.StatusPass,
		Evidence: fmt.Sprintf("%d installed packages match the SBOM exactly", len(installed)),
	}, nil
}

// maskedUnitsCheck builds a check that every named systemd unit is masked:
// /etc/systemd/system/<unit> is a symlink to /dev/null.
func maskedUnitsCheck(units ...string) checks.Func {
	return func(_ context.Context, _ checks.Target, in checks.Inputs) (checks.Outcome, error) {
		var unmasked []string
		for _, unit := range units {
			target, err := in.FS.ReadLink("/etc/systemd/system/" + unit)
			if err != nil || target != "/dev/null" {
				unmasked = append(unmasked, unit)
			}
		}
		if len(unmasked) > 0 {
			return checks.Outcome{
				Status:   checks.StatusFail,
				Evidence: "not masked: " + strings.Join(unmasked, ", "),
			}, nil
		}
		return checks.Outcome{
			Status:   checks.StatusPass,
			Evidence: "masked: " + strings.Join(units, ", "),
		}, nil
	}
}

// checkEtcdDir (HARD-003): /var/lib/etcd exists, mode 0700, owned etcd:etcd —
// what CIS 1.1.12 checks on the running node, asserted at ship time.
func checkEtcdDir(_ context.Context, _ checks.Target, in checks.Inputs) (checks.Outcome, error) {
	st, err := in.FS.Stat("/var/lib/etcd")
	if err != nil {
		return checks.Outcome{Status: checks.StatusFail, Evidence: "/var/lib/etcd: " + err.Error()}, nil
	}
	owner, err := resolveName(in.FS, "/etc/passwd", st.UID)
	if err != nil {
		return checks.Outcome{}, err
	}
	group, err := resolveName(in.FS, "/etc/group", st.GID)
	if err != nil {
		return checks.Outcome{}, err
	}
	if st.Perm != 0o700 || owner != "etcd" || group != "etcd" {
		return checks.Outcome{
			Status:   checks.StatusFail,
			Evidence: fmt.Sprintf("/var/lib/etcd is %04o %s:%s, want 0700 etcd:etcd", st.Perm, owner, group),
		}, nil
	}
	return checks.Outcome{Status: checks.StatusPass, Evidence: "/var/lib/etcd is 0700 etcd:etcd"}, nil
}

// resolveName maps a uid or gid to its name through the image's own
// /etc/passwd or /etc/group (both use "name:x:id:" colon layout).
func resolveName(fs checks.ImageFS, file string, id int) (string, error) {
	content, err := fs.Cat(file)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", file, err)
	}
	want := fmt.Sprintf("%d", id)
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Split(line, ":")
		if len(fields) >= 3 && fields[2] == want {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("%s: no entry for id %d", file, id)
}

// kernelFlavor is the expected kernel package per architecture (SPEC.md).
var kernelFlavor = map[string]string{
	"amd64": "linux-nvidia",
	"arm64": "linux-nvidia-64k",
}

// checkKernelFlavor (HARD-004): the NVIDIA-tuned kernel flavor is installed,
// not the generic default.
func checkKernelFlavor(_ context.Context, t checks.Target, in checks.Inputs) (checks.Outcome, error) {
	flavor, ok := kernelFlavor[t.Arch]
	if !ok {
		return checks.Outcome{}, fmt.Errorf("no expected kernel flavor for architecture %q", t.Arch)
	}
	installed, err := installedPackages(in.FS)
	if err != nil {
		return checks.Outcome{}, err
	}
	if _, ok := installed[flavor]; ok {
		return checks.Outcome{Status: checks.StatusPass, Evidence: "kernel flavor " + flavor + " installed"}, nil
	}
	for name := range installed {
		if strings.HasPrefix(name, "linux-image-") && strings.Contains(name, "-nvidia") {
			return checks.Outcome{Status: checks.StatusPass, Evidence: "NVIDIA kernel image " + name + " installed"}, nil
		}
	}
	return checks.Outcome{
		Status:   checks.StatusFail,
		Evidence: fmt.Sprintf("kernel flavor %s not installed and no linux-image-*-nvidia* package found", flavor),
	}, nil
}

// calicoManifestPrefixes are the Calico manifests package name generations:
// nvke-cni-* after the nke→nvke rename (#1548), nke-cni-* on releases up to
// v0.2.0. The package name carries its own version
// (nke-cni-calico-manifests-3.30.1 with dpkg Version 1 on real images —
// calibrated against the v0.2.0-rc.1 SBOM), so the match is by prefix.
var calicoManifestPrefixes = []string{"nvke-cni-calico-manifests", "nke-cni-calico-manifests"}

// checkCalicoManifests (SPEC-004): the Calico manifests package used during
// bootstrap is installed.
func checkCalicoManifests(_ context.Context, _ checks.Target, in checks.Inputs) (checks.Outcome, error) {
	installed, err := installedPackages(in.FS)
	if err != nil {
		return checks.Outcome{}, err
	}
	for name := range installed {
		for _, prefix := range calicoManifestPrefixes {
			if strings.HasPrefix(name, prefix) {
				return checks.Outcome{Status: checks.StatusPass, Evidence: name + " installed"}, nil
			}
		}
	}
	return checks.Outcome{Status: checks.StatusFail, Evidence: "no nvke-cni-calico-manifests (or pre-rename nke-cni-calico-manifests) package is installed"}, nil
}

// checkLUKSPresent (FDE-001): an image whose name declares the fde variant
// actually carries a LUKS container.
func checkLUKSPresent(_ context.Context, _ checks.Target, in checks.Inputs) (checks.Outcome, error) {
	if !in.FS.HasLUKS() {
		return checks.Outcome{Status: checks.StatusFail, Evidence: "fde target but the image carries no LUKS container"}, nil
	}
	return checks.Outcome{Status: checks.StatusPass, Evidence: "LUKS container present"}, nil
}

// Merge combines implementation maps; a duplicate ID is a programming error.
func Merge(maps ...map[string]checks.Func) map[string]checks.Func {
	out := make(map[string]checks.Func)
	for _, m := range maps {
		for id, fn := range m {
			if _, dup := out[id]; dup {
				panic("duplicate check implementation: " + id)
			}
			out[id] = fn
		}
	}
	return out
}
