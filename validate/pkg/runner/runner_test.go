// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/validate/pkg/report"
)

// fixtureOptions builds a digest-only run over a crafted image whose digest
// matches its checksums entry.
func fixtureOptions(t *testing.T) Options {
	t.Helper()
	dir := t.TempDir()
	const artifact = "nke-node-fde-ubuntu-24.04.5-amd64.qcow2"
	content := []byte("runner fixture bytes")
	image := filepath.Join(dir, artifact)
	require.NoError(t, os.WriteFile(image, content, 0o600))
	sum := sha256.Sum256(content)
	checksums := filepath.Join(dir, "checksums.txt")
	require.NoError(t, os.WriteFile(checksums, fmt.Appendf(nil, "%s  %s\n", hex.EncodeToString(sum[:]), artifact), 0o600))
	return Options{
		ImagePath:     image,
		ChecksumsPath: checksums,
		RegistryPath:  filepath.Join("..", "..", "checks.yaml"),
		Variant:       "fde",
		Arch:          "amd64",
		Ubuntu:        "24.04.5",
		Inspect:       "off",
	}
}

func sink(t *testing.T) *os.File {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "out"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// TestUnknownFormatRejectsBeforeSideEffects pins the round-3 regression: an
// invalid format must fail before any verdict is signed or written.
func TestUnknownFormatRejectsBeforeSideEffects(t *testing.T) {
	opts := fixtureOptions(t)
	opts.Format = "xml"
	opts.EmitVerdictPath = filepath.Join(t.TempDir(), "verdict.json")
	opts.SignKeyPath = filepath.Join(t.TempDir(), "missing.key")

	exit, err := Run(context.Background(), opts, sink(t))
	assert.Equal(t, report.ExitError, exit)
	require.ErrorContains(t, err, "unknown report format")
	_, statErr := os.Stat(opts.EmitVerdictPath)
	assert.True(t, os.IsNotExist(statErr), "no verdict may exist behind an exit-2 usage error")
}

// TestVerdictPathMustNotAliasTheReportStream pins destination separation:
// both documents interleaved in one sink is neither a report nor a verdict.
func TestVerdictPathMustNotAliasTheReportStream(t *testing.T) {
	opts := fixtureOptions(t)
	out := sink(t)
	opts.EmitVerdictPath = out.Name()
	opts.SignKeyPath = filepath.Join(t.TempDir(), "missing.key")

	exit, err := Run(context.Background(), opts, out)
	assert.Equal(t, report.ExitError, exit)
	require.ErrorContains(t, err, "aliases the report output")
}

// TestRunPassesOnTheFixture keeps the happy path pinned at this level too.
func TestRunPassesOnTheFixture(t *testing.T) {
	exit, err := Run(context.Background(), fixtureOptions(t), sink(t))
	require.NoError(t, err)
	assert.Equal(t, report.ExitPass, exit)
}

// TestVerdictDeviceAliasIsCaughtAtOpenTime pins the descriptor-level guard: a
// device path that resolves to the report sink only at open time (e.g.
// /dev/stdout, /dev/fd/N) must yield exit 2 and one error-carrying report,
// never a concatenated envelope.
func TestVerdictDeviceAliasIsCaughtAtOpenTime(t *testing.T) {
	opts := fixtureOptions(t)
	opts.Format = "json"
	out := sink(t)
	opts.EmitVerdictPath = fmt.Sprintf("/dev/fd/%d", out.Fd())
	key := writeSigningKey(t)
	opts.SignKeyPath = key

	exit, err := Run(context.Background(), opts, out)
	require.NoError(t, err, "a post-report failure rides the report, it is not returned")
	assert.Equal(t, report.ExitError, exit)

	data, err := os.ReadFile(out.Name())
	require.NoError(t, err)
	var rep report.Report
	require.NoError(t, json.Unmarshal(data, &rep), "stdout must hold exactly one JSON document")
	assert.Contains(t, rep.RunError, "aliases the report output")
}

func writeSigningKey(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "sign.key")
	require.NoError(t, os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600))
	return path
}

// TestBundleModeRequiresTrustedRoot pins the out-of-band trust-root contract
// at the runner boundary: bundle mode without a pinned root is a usage error
// before any work, and a root supplied to a digest-only run (which verifies
// no evidence) is one too.
func TestBundleModeRequiresTrustedRoot(t *testing.T) {
	opts := fixtureOptions(t)
	opts.BundleDir = filepath.Dir(opts.ChecksumsPath)
	opts.ChecksumsPath = ""

	exit, err := Run(context.Background(), opts, sink(t))
	assert.Equal(t, report.ExitError, exit)
	require.ErrorContains(t, err, "trusted root")
	require.ErrorContains(t, err, "out-of-band")
}

func TestTrustedRootRejectedInDigestOnlyMode(t *testing.T) {
	opts := fixtureOptions(t)
	opts.TrustedRootPath = filepath.Join(t.TempDir(), "root.json")

	exit, err := Run(context.Background(), opts, sink(t))
	assert.Equal(t, report.ExitError, exit)
	require.ErrorContains(t, err, "only applies to bundle mode")
}
