// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package static implements the T-static check tier: facts that need no image
// introspection — digests, release-checksums membership, artifact-name
// identity, and the target BOM against the release SBOM. The inputs come from
// the release's attested checksums.txt, whose own signature pkg/bundle
// verifies in --bundle mode; digest-only (--checksums) runs report that
// unverified trust visibly through INT-002.
package static

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/dsx-ai-factory/nvke/validate/pkg/checks"
)

// Impls returns the static-tier implementations keyed by check ID, for
// checks.NewEngine.
func Impls() map[string]checks.Func {
	return map[string]checks.Func{
		"INT-001":    checkImageDigest,
		"INT-002":    checkEvidenceVerified,
		"COMPAT-001": checkArtifactIdentity,
		"COMPAT-002": checkBOMMatchesSBOM,
	}
}

// HashFile computes the bare-hex sha256 of a file, streaming.
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hashing %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ParseChecksums reads a sha256sum-format file (`<64 hex>  <name>` per line,
// the format os-image-artifact.yml publishes) into a name → digest map.
func ParseChecksums(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading checksums: %w", err)
	}
	sums, err := ParseChecksumsBytes(raw)
	if err != nil {
		return nil, fmt.Errorf("checksums %s: %w", path, err)
	}
	return sums, nil
}

// ParseChecksumsBytes parses sha256sum format from one byte snapshot, so
// callers can authenticate and parse the same bytes. A malformed line is an
// error: a file that parses partially would turn a missing artifact into a
// silent skip.
func ParseChecksumsBytes(raw []byte) (map[string]string, error) {
	sums := make(map[string]string)
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		digest, name, ok := strings.Cut(text, "  ")
		name = strings.TrimPrefix(name, "*") // sha256sum binary-mode marker
		if !ok || !isHexDigest(digest) || name == "" {
			return nil, fmt.Errorf("line %d: not sha256sum format", line)
		}
		if prev, dup := sums[name]; dup && prev != digest {
			return nil, fmt.Errorf("line %d: conflicting digests for %s", line, name)
		}
		sums[name] = digest
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading: %w", err)
	}
	if len(sums) == 0 {
		return nil, fmt.Errorf("no entries")
	}
	return sums, nil
}

func isHexDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// artifactName matches the release naming scheme the os-image workflow
// publishes: nvke-node-fde-ubuntu-24.04.5-amd64.qcow2, and the nke-node-*
// spelling that releases up to v0.2.0 carry (the nke→nvke rename, #1548).
var artifactName = regexp.MustCompile(`^nv?ke-node-(fde|non-fde)-ubuntu-([0-9][0-9.]*)-(amd64|arm64)\.qcow2$`)

// Identity is what an artifact's release name declares about it.
type Identity struct {
	Variant checks.Variant
	Ubuntu  string
	Arch    string
}

// IdentityFromName parses an artifact release name into its declared identity.
func IdentityFromName(name string) (Identity, error) {
	m := artifactName.FindStringSubmatch(name)
	if m == nil {
		return Identity{}, fmt.Errorf("artifact name %q does not match the release naming scheme", name)
	}
	return Identity{Variant: checks.Variant(m[1]), Ubuntu: m[2], Arch: m[3]}, nil
}

// checkImageDigest (INT-001): the image file's sha256 equals the digest the
// release checksums record under the target's artifact name.
func checkImageDigest(_ context.Context, t checks.Target, in checks.Inputs) (checks.Outcome, error) {
	want, ok := in.Checksums[t.ImageName]
	if !ok {
		return checks.Outcome{
			Status:   checks.StatusFail,
			Evidence: fmt.Sprintf("artifact %s not listed in release checksums", t.ImageName),
		}, nil
	}
	if in.ImageSHA256 != want {
		return checks.Outcome{
			Status:   checks.StatusFail,
			Evidence: fmt.Sprintf("image sha256 %s != recorded %s for %s", in.ImageSHA256, want, t.ImageName),
		}, nil
	}
	return checks.Outcome{
		Status:   checks.StatusPass,
		Evidence: fmt.Sprintf("sha256 %s matches release checksums entry %s", want, t.ImageName),
	}, nil
}

// checkArtifactIdentity (COMPAT-001): the variant, Ubuntu release and
// architecture the artifact's name declares are the ones being validated
// against — a mismatch means the wrong image is under test.
func checkArtifactIdentity(_ context.Context, t checks.Target, _ checks.Inputs) (checks.Outcome, error) {
	id, err := IdentityFromName(t.ImageName)
	if err != nil {
		return checks.Outcome{}, err
	}
	var mismatches []string
	if id.Variant != t.Variant {
		mismatches = append(mismatches, fmt.Sprintf("variant %s != target %s", id.Variant, t.Variant))
	}
	if id.Arch != t.Arch {
		mismatches = append(mismatches, fmt.Sprintf("arch %s != target %s", id.Arch, t.Arch))
	}
	if id.Ubuntu != t.Ubuntu {
		mismatches = append(mismatches, fmt.Sprintf("ubuntu %s != target %s", id.Ubuntu, t.Ubuntu))
	}
	if len(mismatches) > 0 {
		return checks.Outcome{Status: checks.StatusFail, Evidence: strings.Join(mismatches, "; ")}, nil
	}
	return checks.Outcome{
		Status:   checks.StatusPass,
		Evidence: fmt.Sprintf("name declares %s/%s/ubuntu-%s, matching the target", id.Variant, id.Arch, id.Ubuntu),
	}, nil
}

// bomFile is the target BOM document: the component versions the validation
// target expects on the image.
type bomFile struct {
	Components map[string]string `yaml:"components"`
}

// Deb package names are lowercase alphanumerics plus +.- (Debian policy §5.6.1,
// minimum two characters); versions start with a digit or an epoch. Enforcing
// the charsets here means a BOM entry can later be embedded in a probe command
// without quoting concerns.
var (
	debName    = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]+$`)
	debVersion = regexp.MustCompile(`^[0-9][A-Za-z0-9.+~:-]*$`)
)

// ParseBOM reads a target BOM YAML (`components: {name: version}`) into a
// package name → required version map.
func ParseBOM(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading target BOM: %w", err)
	}
	bom, err := ParseBOMBytes(raw)
	if err != nil {
		return nil, fmt.Errorf("target BOM %s: %w", path, err)
	}
	return bom, nil
}

// ParseBOMBytes validates a target BOM from one byte snapshot, so callers
// hash and parse the same bytes. Unknown fields, empty documents and entries
// outside the deb charsets are errors — a partially parsed BOM would turn a
// missing requirement into a silent pass.
func ParseBOMBytes(raw []byte) (map[string]string, error) {
	var doc bomFile
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("parsing: %w", err)
	}
	if len(doc.Components) == 0 {
		return nil, fmt.Errorf("no components")
	}
	for name, version := range doc.Components {
		if !debName.MatchString(name) {
			return nil, fmt.Errorf("%q is not a deb package name", name)
		}
		if !debVersion.MatchString(version) {
			return nil, fmt.Errorf("component %s: %q is not a deb version", name, version)
		}
	}
	return doc.Components, nil
}

// checkBOMMatchesSBOM (COMPAT-002): every component the target BOM names is in
// the release SBOM at the required version. The image ships no kubeadm or
// kubelet — the node agent installs the Kubernetes stack after provisioning —
// so target compatibility is carried by the baked-in NVKE components, and the
// attested SBOM is the release's own statement of their versions.
func checkBOMMatchesSBOM(_ context.Context, _ checks.Target, in checks.Inputs) (checks.Outcome, error) {
	if in.BOM == nil {
		return checks.Outcome{
			Status:   checks.StatusSkip,
			Reason:   checks.SkipInputMissing,
			Evidence: "no target BOM supplied (--target-bom)",
		}, nil
	}
	if in.SBOMPackages == nil {
		return checks.Outcome{}, fmt.Errorf("target BOM given but no release SBOM is available (needs --bundle with this artifact's SBOM)")
	}
	var drift []string
	for _, name := range sortedKeys(in.BOM) {
		want := in.BOM[name]
		got, ok := in.SBOMPackages[name]
		switch {
		case !ok:
			drift = append(drift, fmt.Sprintf("%s: bom %s, not in SBOM", name, want))
		case got != want:
			drift = append(drift, fmt.Sprintf("%s: bom %s, sbom %s", name, want, got))
		}
	}
	if len(drift) > 0 {
		return checks.Outcome{
			Status:   checks.StatusFail,
			Evidence: fmt.Sprintf("%d component(s) off the target BOM: %s", len(drift), strings.Join(drift, "; ")),
		}, nil
	}
	return checks.Outcome{
		Status:   checks.StatusPass,
		Evidence: fmt.Sprintf("all %d BOM components are in the release SBOM at required versions", len(in.BOM)),
	}, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// checkEvidenceVerified (INT-002): the release evidence attestation verified
// offline against the pinned trusted root, and its subject is this bundle's
// checksums.txt. A digest-only run (no bundle) is an explicit caller choice
// and reports a skip with the reason — visible in the report, never a pass.
func checkEvidenceVerified(_ context.Context, _ checks.Target, in checks.Inputs) (checks.Outcome, error) {
	ev := in.Evidence
	if !ev.Checked {
		return checks.Outcome{
			Status:   checks.StatusSkip,
			Reason:   checks.SkipInputMissing,
			Evidence: "evidence verification not attempted: digest-only (--checksums) mode",
		}, nil
	}
	if ev.Err != nil {
		return checks.Outcome{Status: checks.StatusFail, Evidence: ev.Err.Error()}, nil
	}
	return checks.Outcome{
		Status:   checks.StatusPass,
		Evidence: fmt.Sprintf("attestation verified; signer %s", ev.SignerSAN),
	}, nil
}
