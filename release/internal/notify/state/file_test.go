// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	f := File{Dir: filepath.Join(t.TempDir(), "nested")}

	if got, err := f.Get("v1", "C1"); err != nil || got != "" {
		t.Fatalf("Get before Set = %q, %v; want empty and no error", got, err)
	}
	if err := f.Set("v1", "C1", "1793894851.752799"); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.Get("v1", "C1"); got != "1793894851.752799" {
		t.Errorf("Get = %q", got)
	}
	// Another channel, and another conversation, are separate threads.
	if got, _ := f.Get("v1", "C2"); got != "" {
		t.Errorf("another channel read %q", got)
	}
	if got, _ := f.Get("v2", "C1"); got != "" {
		t.Errorf("another key read %q", got)
	}
}

// A cache that was truncated or edited must read as no thread, not as an error.
func TestCorruptStateIsNoThread(t *testing.T) {
	f := File{Dir: t.TempDir()}
	for _, content := range []string{"", "garbage", "12.34.56", "1793894851.752799; rm -rf /"} {
		if err := os.WriteFile(f.path("k", "C1"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if got, err := f.Get("k", "C1"); err != nil || got != "" {
			t.Errorf("Get(%q) = %q, %v; want empty and no error", content, got, err)
		}
	}
}

func TestSetRejectsANonTimestamp(t *testing.T) {
	if err := (File{Dir: t.TempDir()}).Set("k", "C1", "not-a-ts"); err == nil {
		t.Error("Set accepted a value that is not a Slack timestamp")
	}
}

// A key is a tag name or whatever the caller set, so it must not leave Dir.
func TestKeyStaysInsideDir(t *testing.T) {
	dir := t.TempDir()
	f := File{Dir: dir}
	for _, key := range []string{"../../escape", "/etc/passwd", "..", "a/b", "v1~C1"} {
		if err := f.Set(key, "C1", "1.2"); err != nil {
			t.Fatalf("Set(%q): %v", key, err)
		}
		rel, err := filepath.Rel(dir, f.path(key, "C1"))
		if err != nil || filepath.Dir(rel) != "." {
			t.Errorf("key %q wrote to %q, outside the directory", key, rel)
		}
	}
	// "v1" in channel "C1" and "v1~C1" in no channel must not be the same file.
	if f.path("v1", "C1") == f.path("v1~C1", "") {
		t.Error("two different conversations share a file")
	}
}
