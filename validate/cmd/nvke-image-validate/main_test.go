// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/validate/pkg/checks/static"
	"github.com/dsx-ai-factory/nvke/validate/pkg/report"
	"github.com/dsx-ai-factory/nvke/validate/pkg/verify"
)

const artifact = "nke-node-fde-ubuntu-24.04.5-amd64.qcow2"

// fixture builds an image file + checksums.txt recording digest (tampered
// flips a byte of the recorded digest) and returns the common CLI args.
func fixture(t *testing.T, tampered bool) []string {
	t.Helper()
	dir := t.TempDir()
	content := []byte("not a real qcow2, and that is fine for the static tier")
	image := filepath.Join(dir, artifact)
	require.NoError(t, os.WriteFile(image, content, 0o644))
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	if tampered {
		digest = "0" + digest[1:]
		if digest == hex.EncodeToString(sum[:]) {
			digest = "1" + digest[1:]
		}
	}
	checksums := filepath.Join(dir, "checksums.txt")
	require.NoError(t, os.WriteFile(checksums, []byte(fmt.Sprintf("%s  %s\n", digest, artifact)), 0o644))
	return []string{
		"--image", image,
		"--checksums", checksums,
		"--checks", filepath.Join("..", "..", "checks.yaml"),
		"--target-variant", "fde",
		"--target-arch", "amd64",
		"--target-ubuntu", "24.04.5",
	}
}

func devNull(t *testing.T) *os.File {
	t.Helper()
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func TestRun(t *testing.T) {
	tests := []struct {
		name string
		args func(t *testing.T) []string
		want int
	}{
		{
			name: "valid image passes",
			args: func(t *testing.T) []string { return fixture(t, false) },
			want: report.ExitPass,
		},
		{
			name: "tampered checksums fail blocking",
			args: func(t *testing.T) []string { return fixture(t, true) },
			want: report.ExitFail,
		},
		{
			name: "identity mismatch fails blocking",
			args: func(t *testing.T) []string {
				args := fixture(t, false)
				for i, a := range args {
					if a == "fde" {
						args[i] = "non-fde"
					}
				}
				return args
			},
			want: report.ExitFail,
		},
		{
			name: "missing required flag is a usage error",
			args: func(_ *testing.T) []string { return []string{"--image", "x.qcow2"} },
			want: report.ExitError,
		},
		{
			name: "unreadable image is an error, not a fail",
			args: func(t *testing.T) []string {
				args := fixture(t, false)
				args[1] = filepath.Join(t.TempDir(), "missing.qcow2")
				return args
			},
			want: report.ExitError,
		},
		{
			name: "version flag exits 0",
			args: func(_ *testing.T) []string { return []string{"--version"} },
			want: report.ExitPass,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sink := devNull(t)
			assert.Equal(t, tt.want, run(tt.args(t), sink, sink))
		})
	}
}

func TestRunJSONFormat(t *testing.T) {
	args := append(fixture(t, false), "--format", "json")
	out := filepath.Join(t.TempDir(), "out.json")
	f, err := os.Create(out)
	require.NoError(t, err)
	assert.Equal(t, report.ExitPass, run(args, f, devNull(t)))
	require.NoError(t, f.Close())
	data, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"schemaVersion": "1"`)
	assert.Contains(t, string(data), `"INT-001"`)
}

const (
	realBundle = "../../testdata/release-v0.2.0-rc.1"
	// realTrustedRoot is the test's out-of-band pin; production never
	// discovers it inside the evidence directory.
	realTrustedRoot = realBundle + "/trusted-root.json"
)

// TestRunBundleMode exercises the evidence-verifying path against the vendored
// real release bundle: the evidence verifies (INT-002 pass), while a wrong
// file posing as the image fails INT-001 — so the run exits 1 for the right
// reason, which the JSON report distinguishes.
func TestRunBundleMode(t *testing.T) {
	image := filepath.Join(t.TempDir(), artifact)
	require.NoError(t, os.WriteFile(image, []byte("wrong bytes"), 0o644))
	out := filepath.Join(t.TempDir(), "out.json")
	f, err := os.Create(out)
	require.NoError(t, err)
	code := run([]string{
		"--image", image,
		"--bundle", realBundle,
		"--trusted-root", realTrustedRoot,
		"--checks", filepath.Join("..", "..", "checks.yaml"),
		"--target-variant", "fde", "--target-arch", "amd64", "--target-ubuntu", "24.04.5",
		"--format", "json",
	}, f, devNull(t))
	require.NoError(t, f.Close())
	assert.Equal(t, report.ExitFail, code)
	data, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.Regexp(t, `"id": "INT-002",\s*"status": "pass"`, string(data))
	assert.Regexp(t, `"id": "INT-001",\s*"status": "fail"`, string(data))
}

func TestRunRequiresExactlyOneEvidenceSource(t *testing.T) {
	base := fixture(t, false)
	both := append(append([]string{}, base...), "--bundle", realBundle)
	neither := make([]string, 0, len(base)-2)
	for i := 0; i < len(base); i++ {
		if base[i] == "--checksums" {
			i++
			continue
		}
		neither = append(neither, base[i])
	}
	sink := devNull(t)
	assert.Equal(t, report.ExitError, run(both, sink, sink))
	assert.Equal(t, report.ExitError, run(neither, sink, sink))
}

// TestRunBundleModeRequiresTrustedRoot pins the out-of-band contract: bundle
// mode without an operator-pinned root is a usage error before any work, and
// the root is never discovered inside the evidence directory (the vendored
// fixture dir contains a trusted-root.json that must NOT satisfy this).
func TestRunBundleModeRequiresTrustedRoot(t *testing.T) {
	image := filepath.Join(t.TempDir(), artifact)
	require.NoError(t, os.WriteFile(image, []byte("wrong bytes"), 0o644))
	sink := devNull(t)
	code := run([]string{
		"--image", image,
		"--bundle", realBundle,
		"--checks", filepath.Join("..", "..", "checks.yaml"),
		"--target-variant", "fde", "--target-arch", "amd64", "--target-ubuntu", "24.04.5",
	}, sink, sink)
	assert.Equal(t, report.ExitError, code)
}

// TestRunTargetBOM exercises --target-bom against the vendored real SBOM:
// COMPAT-002 passes when the BOM names the versions the release SBOM records
// and fails naming the drifted component otherwise — without any image
// introspection.
func TestRunTargetBOM(t *testing.T) {
	image := filepath.Join(t.TempDir(), artifact)
	require.NoError(t, os.WriteFile(image, []byte("wrong bytes"), 0o644))
	for _, tt := range []struct {
		name, agentVersion, wantStatus string
	}{
		{name: "matching BOM passes COMPAT-002", agentVersion: "0.3.0-rc.1", wantStatus: "pass"},
		{name: "drifted BOM fails COMPAT-002", agentVersion: "9.9.9", wantStatus: "fail"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			bomPath := filepath.Join(t.TempDir(), "bom.yaml")
			require.NoError(t, os.WriteFile(bomPath, []byte("components:\n  nke-agent: "+tt.agentVersion+"\n"), 0o644))
			out := filepath.Join(t.TempDir(), "out.json")
			f, err := os.Create(out)
			require.NoError(t, err)
			code := run([]string{
				"--image", image,
				"--bundle", realBundle,
				"--trusted-root", realTrustedRoot,
				"--checks", filepath.Join("..", "..", "checks.yaml"),
				"--target-variant", "fde", "--target-arch", "amd64", "--target-ubuntu", "24.04.5",
				"--target-bom", bomPath,
				"--inspect", "off",
				"--format", "json",
			}, f, devNull(t))
			require.NoError(t, f.Close())
			assert.Equal(t, report.ExitFail, code, "wrong image bytes keep INT-001 failing regardless of the BOM")
			data, err := os.ReadFile(out)
			require.NoError(t, err)
			assert.Regexp(t, `"id": "COMPAT-002",\s*"status": "`+tt.wantStatus+`"`, string(data))
		})
	}
}

// TestRunEmitsSignedVerdict exercises --emit-verdict end to end: the emitted
// DSSE attestation verifies offline against the signer's public key and the
// image's digest, and carries the run's outcome.
func TestRunEmitsSignedVerdict(t *testing.T) {
	dir := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	keyPath := filepath.Join(dir, "sign.key")
	require.NoError(t, os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600))

	for _, tt := range []struct {
		name        string
		tampered    bool
		wantExit    int
		wantOutcome string
	}{
		{name: "passing run emits a pass verdict", tampered: false, wantExit: report.ExitPass, wantOutcome: verify.OutcomePass},
		{name: "failing run still emits its verdict", tampered: true, wantExit: report.ExitFail, wantOutcome: verify.OutcomeFail},
	} {
		t.Run(tt.name, func(t *testing.T) {
			verdictPath := filepath.Join(t.TempDir(), "verdict.json")
			args := append(fixture(t, tt.tampered), "--emit-verdict", verdictPath, "--sign-key", keyPath)
			sink := devNull(t)
			assert.Equal(t, tt.wantExit, run(args, sink, sink))

			envelope, err := os.ReadFile(verdictPath)
			require.NoError(t, err)
			var imagePath string
			for i, a := range args {
				if a == "--image" {
					imagePath = args[i+1]
				}
			}
			sum, err := static.HashFile(imagePath)
			require.NoError(t, err)
			st, err := verify.Verify(envelope, sum, verify.Policy{Keys: []crypto.PublicKey{&key.PublicKey}}, time.Now())
			require.NoError(t, err)
			assert.Equal(t, tt.wantOutcome, st.Predicate.Outcome)
			assert.NotEmpty(t, st.Predicate.CheckSetVersion)
			assert.NotEmpty(t, st.Predicate.Results)

			// The verdict binds its policy inputs and names its evidence mode.
			assert.Equal(t, verify.EvidenceModeDigestOnly, st.Predicate.EvidenceMode, "a --checksums run must say so in the verdict")
			assert.Len(t, st.Predicate.CheckSetSHA256, 64)

			// Omitted validation is machine-readable: this digest-only run
			// skipped INT-002 for a missing input, and a consumer profile that
			// requires INT-002 must therefore reject the verdict even though
			// its outcome is signed and authentic.
			var int002 verify.CheckResult
			for _, r := range st.Predicate.Results {
				if r.ID == "INT-002" {
					int002 = r
				}
			}
			assert.Equal(t, "skip", int002.Status)
			assert.Equal(t, "input-missing", int002.Reason)
			_, err = verify.Verify(envelope, sum, verify.Policy{
				Keys:           []crypto.PublicKey{&key.PublicKey},
				RequiredChecks: []string{"INT-001", "INT-002"},
			}, time.Now())
			wantProfileErr := "omitted validation" // the pass verdict still skipped INT-002
			if tt.tampered {
				wantProfileErr = "status fail" // the fail verdict is rejected for the failed check
			}
			require.ErrorContains(t, err, wantProfileErr,
				"an authentic verdict must not satisfy a qualification profile it does not meet")
		})
	}
}

// TestRunSetupErrorEmitsStructuredReport pins that a run dying before any
// check still answers the pipeline in the report schema on stdout.
func TestRunSetupErrorEmitsStructuredReport(t *testing.T) {
	args := fixture(t, false)
	args[1] = filepath.Join(t.TempDir(), "missing.qcow2")
	out := filepath.Join(t.TempDir(), "out.json")
	f, err := os.Create(out)
	require.NoError(t, err)
	assert.Equal(t, report.ExitError, run(append(args, "--format", "json"), f, devNull(t)))
	require.NoError(t, f.Close())
	data, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"schemaVersion": "1"`)
	assert.Contains(t, string(data), `"runError"`)
	assert.Regexp(t, `"error": 1`, string(data))
}

func TestRunVerdictFlagsAreRequiredTogether(t *testing.T) {
	sink := devNull(t)
	args := append(fixture(t, false), "--emit-verdict", filepath.Join(t.TempDir(), "v.json"))
	assert.Equal(t, report.ExitError, run(args, sink, sink))
	args = append(fixture(t, false), "--sign-key", filepath.Join(t.TempDir(), "missing.key"))
	assert.Equal(t, report.ExitError, run(args, sink, sink))
}

// TestRunEmissionFailureIsOneDocument pins the pipeline contract: a run whose
// checks completed but whose verdict could not be signed emits exactly one
// JSON document carrying both the results and the operational error, exit 2.
func TestRunEmissionFailureIsOneDocument(t *testing.T) {
	args := append(fixture(t, false),
		"--format", "json",
		"--emit-verdict", filepath.Join(t.TempDir(), "v.json"),
		"--sign-key", filepath.Join(t.TempDir(), "missing.key"))
	out := filepath.Join(t.TempDir(), "out.json")
	f, err := os.Create(out)
	require.NoError(t, err)
	assert.Equal(t, report.ExitError, run(args, f, devNull(t)))
	require.NoError(t, f.Close())
	data, err := os.ReadFile(out)
	require.NoError(t, err)
	var rep report.Report
	require.NoError(t, json.Unmarshal(data, &rep), "stdout must be exactly one JSON document")
	assert.NotEmpty(t, rep.Results, "the completed check results must survive the emission failure")
	assert.Contains(t, rep.RunError, "emitting verdict")
	assert.Equal(t, report.ExitError, rep.ExitCode())
}
