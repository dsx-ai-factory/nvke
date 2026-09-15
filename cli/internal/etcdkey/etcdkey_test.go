// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package etcdkey

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
)

const sentinelKey = "sentinel-key-material-that-must-not-leak"

func testData() map[string][]byte {
	return map[string][]byte{
		FieldActive:    []byte(sentinelKey),
		"active-since": []byte("1700000000"),
	}
}

// TestManifest pins the shape a later controller install depends on: CWE's
// own name for the Secret, no namespace, the cluster name where an operator
// can find it, and every data field carried through — not just the active
// key, since rotation state travels with it.
func TestManifest(t *testing.T) {
	id := uuid.New()
	manifest, err := Manifest(id, "my-site", testData())
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}

	var got corev1.Secret
	if err := json.Unmarshal(manifest, &got); err != nil {
		t.Fatalf("manifest is not a Secret: %v\n%s", err, manifest)
	}
	if got.APIVersion != "v1" || got.Kind != "Secret" || got.Type != corev1.SecretTypeOpaque {
		t.Errorf("apiVersion/kind/type = %q/%q/%q, want v1/Secret/Opaque", got.APIVersion, got.Kind, got.Type)
	}
	if want := "etcd-encryption-key-" + id.String(); got.Name != want {
		t.Errorf("metadata.name = %q, want %q", got.Name, want)
	}
	if got.Namespace != "" {
		t.Errorf("metadata.namespace = %q, want none: the target namespace is chosen at apply time", got.Namespace)
	}
	if got.Annotations[ClusterNameAnnotation] != "my-site" {
		t.Errorf("annotation %s = %q, want %q", ClusterNameAnnotation, got.Annotations[ClusterNameAnnotation], "my-site")
	}
	// The zero-valued parts of ObjectMeta must not print: an operator reads
	// this file, and a "creationTimestamp: null" reads as a defect.
	for _, unwanted := range []string{"creationTimestamp", "\"namespace\"", "\"status\""} {
		if strings.Contains(string(manifest), unwanted) {
			t.Errorf("manifest carries %s:\n%s", unwanted, manifest)
		}
	}
	if string(got.Data[FieldActive]) != sentinelKey {
		t.Errorf("data.active did not round-trip")
	}
	if string(got.Data["active-since"]) != "1700000000" {
		t.Errorf("data.active-since was dropped; rotation state has to travel with the key")
	}
}

func TestManifestRequiresActiveKey(t *testing.T) {
	_, err := Manifest(uuid.New(), "my-site", map[string][]byte{"active-since": []byte("1")})
	if err == nil || !strings.Contains(err.Error(), FieldActive) {
		t.Errorf("Manifest without an active key: err = %v, want one naming %q", err, FieldActive)
	}
}

func TestWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys", "site-etcd-encryption-key.json")
	manifest := []byte(`{"data":{"active":"` + sentinelKey + `"}}` + "\n")

	if err := Write(path, manifest); err != nil {
		t.Fatalf("Write: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("file mode = %o, want 0600", mode)
	}
	if dirInfo, err := os.Stat(filepath.Dir(path)); err == nil {
		if mode := dirInfo.Mode().Perm(); mode != 0o700 {
			t.Errorf("created directory mode = %o, want 0700", mode)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(manifest) {
		t.Errorf("on-disk content differs from what was written")
	}

	// Nothing else left behind: the temporary file is linked into place and
	// removed, not renamed, so a stray would mean the cleanup did not run.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v, want only the key file", names)
	}
}

// TestWriteNeverOverwrites: the file at the path is either an earlier
// bootstrap's key or something planted there. Both are refused, and the
// refusal does not carry the key that was about to be written.
func TestWriteNeverOverwrites(t *testing.T) {
	dir := t.TempDir()

	for name, plant := range map[string]func(path string){
		"regular file": func(path string) {
			if err := os.WriteFile(path, []byte("earlier"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"symlink": func(path string) {
			target := filepath.Join(dir, "elsewhere")
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		},
	} {
		path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-"))
		plant(path)

		err := Write(path, []byte(sentinelKey))
		if !errors.Is(err, ErrExists) {
			t.Errorf("%s: err = %v, want ErrExists", name, err)
		}
		if err != nil && strings.Contains(err.Error(), sentinelKey) {
			t.Errorf("%s: error carries the key material", name)
		}
		if name == "symlink" {
			if _, statErr := os.Stat(filepath.Join(dir, "elsewhere")); statErr == nil {
				t.Errorf("symlink was followed: its target now exists")
			}
		}
	}
}

func TestFingerprint(t *testing.T) {
	a := Fingerprint(testData())
	b := Fingerprint(testData())
	if a != b || len(a) != 64 {
		t.Errorf("Fingerprint = %q / %q, want a stable 64-hex-digit digest", a, b)
	}
	if strings.Contains(a, sentinelKey) {
		t.Error("Fingerprint contains the key")
	}
}
