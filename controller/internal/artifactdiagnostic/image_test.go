// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package artifactdiagnostic

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func imageFixture(t *testing.T, arch string, corrupt bool) string {
	t.Helper()
	blob := func(raw []byte) string { hash := sha256.Sum256(raw); return "sha256:" + hex.EncodeToString(hash[:]) }
	config := []byte(`{"os":"linux","architecture":"` + arch + `"}`)
	manifest, err := json.Marshal(map[string]any{"config": descriptor{Digest: blob(config)}, "layers": []descriptor{}})
	if err != nil {
		t.Fatal(err)
	}
	index, err := json.Marshal(map[string]any{"manifests": []descriptor{{Digest: blob(manifest)}}})
	if err != nil {
		t.Fatal(err)
	}
	entries := map[string][]byte{"oci-layout": []byte(`{"imageLayoutVersion":"1.0.0"}`), "index.json": index, "blobs/sha256/" + blob(manifest)[7:]: manifest, "blobs/sha256/" + blob(config)[7:]: config}
	if corrupt {
		entries["blobs/sha256/"+blob(config)[7:]] = []byte("corrupt")
	}
	var raw bytes.Buffer
	tr := tar.NewWriter(&raw)
	for name, body := range entries {
		if err := tr.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Typeflag: tar.TypeReg, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tr.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "image.tar")
	if err := os.WriteFile(file, raw.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestNativeImageAdmission(t *testing.T) {
	for _, tc := range []struct {
		name, arch    string
		corrupt, fail bool
	}{{"native", nativeArch, false, false}, {"wrong platform", "arm64", false, true}, {"changed blob", nativeArch, true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			index, manifest, config, err := inspectImage(t.Context(), imageFixture(t, tc.arch, tc.corrupt))
			if (err != nil) != tc.fail {
				t.Fatalf("inspect: %v", err)
			}
			if !tc.fail && (index == manifest || manifest == config || !validDigest(index)) {
				t.Fatal("identity families conflated")
			}
		})
	}
}
