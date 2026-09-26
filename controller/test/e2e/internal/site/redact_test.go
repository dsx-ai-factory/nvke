// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package site

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"testing"
)

// The values a run actually places on a node: a Forge token, a registry
// token, and two generated passwords. Long enough to be searched for, which
// real ones are.
const (
	testForgeToken = "nvapi-0123456789abcdefghij"
	testDBPassword = "8f14e45fceea167a5a36dedd4bea2543"
)

func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	// A directory entry, so the rewrite is exercised against a member that
	// carries no content.
	if err := tw.WriteHeader(&tar.Header{Name: "diagnostics/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	for _, closer := range []io.Closer{tw, zw} {
		if err := closer.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

func readTarGz(t *testing.T, data []byte) map[string]string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("the redacted archive is not gzip: %v", err)
	}
	defer func() { _ = zr.Close() }()
	out := map[string]string{}
	tr := tar.NewReader(zr)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("the redacted archive is not a readable tar: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		// A reader that trusted Size over the bytes present -- tar -x does --
		// would truncate or pad, so the header has to agree with the content.
		if header.Size != int64(len(body)) {
			t.Errorf("%s: header says %d bytes, %d present", header.Name, header.Size, len(body))
		}
		out[header.Name] = string(body)
	}
}

func TestRedactArchive(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte(testDBPassword))
	in := tarGz(t, map[string]string{
		// A journal line, which is how a token most plausibly escapes.
		"diagnostics/journal.log": "Sep 26 01:14:02 nke nkectl[913]: POST with token=" + testForgeToken + " failed\n",
		// A Secret as kubectl prints it: the value is base64, not plain.
		"diagnostics/secret.yaml": "data:\n  password: " + encoded + "\n",
		// Nothing to redact, and it must come through byte for byte.
		"diagnostics/versions.txt": "controller v0.1.1-rc.8\nkube 1.36.3\n",
	})

	out, err := redactArchive(in, []string{testForgeToken, testDBPassword})
	if err != nil {
		t.Fatal(err)
	}
	files := readTarGz(t, out)

	if _, ok := files["diagnostics/"]; !ok {
		t.Error("the directory entry did not survive the rewrite")
	}
	if got := files["diagnostics/versions.txt"]; got != "controller v0.1.1-rc.8\nkube 1.36.3\n" {
		t.Errorf("a file with nothing to redact was altered: %q", got)
	}
	for name, secret := range map[string]string{
		"diagnostics/journal.log": testForgeToken,
		"diagnostics/secret.yaml": encoded,
	} {
		if strings.Contains(files[name], secret) {
			t.Errorf("%s still carries the secret: %q", name, files[name])
		}
		if !strings.Contains(files[name], redactedMarker) {
			t.Errorf("%s was not marked as redacted: %q", name, files[name])
		}
	}
	// The surrounding text is what makes the line diagnosable, so only the
	// value should be gone.
	if !strings.Contains(files["diagnostics/journal.log"], "nkectl[913]") {
		t.Errorf("redaction removed more than the secret: %q", files["diagnostics/journal.log"])
	}
}

// An archive with nothing to act on is returned untouched rather than
// recompressed, so a run that placed no secrets cannot have its diagnostics
// damaged by this path.
func TestRedactArchiveWithoutSecrets(t *testing.T) {
	in := tarGz(t, map[string]string{"diagnostics/journal.log": "nothing sensitive\n"})
	for _, secrets := range [][]string{nil, {}, {""}, {"short"}} {
		out, err := redactArchive(in, secrets)
		if err != nil {
			t.Fatalf("secrets %q: %v", secrets, err)
		}
		if !bytes.Equal(in, out) {
			t.Errorf("secrets %q: the archive was rewritten with nothing to redact", secrets)
		}
	}
}

// A value too short to search for is dropped, because replacing it would hit
// unrelated text. Worth pinning: the alternative reads as redaction working.
func TestRedactions(t *testing.T) {
	got := redactions([]string{"tok", "", "nke-tenant", "nke-tenant", testDBPassword})
	want := map[string]bool{
		"nke-tenant": true,
		base64.StdEncoding.EncodeToString([]byte("nke-tenant")): true,
		testDBPassword: true,
		base64.StdEncoding.EncodeToString([]byte(testDBPassword)): true,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d needles, want %d: %q", len(got), len(want), got)
	}
	for _, needle := range got {
		if !want[string(needle)] {
			t.Errorf("unexpected needle %q", needle)
		}
	}
}

func TestRedactArchiveRejectsGarbage(t *testing.T) {
	if _, err := redactArchive([]byte("not a gzip stream"), []string{testForgeToken}); err == nil {
		t.Error("a non-gzip archive was accepted")
	}
}
