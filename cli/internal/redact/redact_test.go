// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package redact

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeValue stands in for a credential. It is deliberately plain.
const fakeValue = "aaaa-bbbb-cccc"

func TestText(t *testing.T) {
	RegisterSecret(fakeValue)
	RegisterSecret("") // ignored: masking "" would mangle every string

	for _, tc := range []struct {
		name, in, want string
	}{
		{"a registered value is masked", "rejected " + fakeValue + " twice " + fakeValue, "rejected <redacted> twice <redacted>"},
		{"a password in a database URL is masked without being registered", "postgres://u:other-pw@h:5432/db", "postgres://u:<redacted>@h:5432/db"},
		{"the postgresql scheme too", "postgresql://u:other-pw@h/db?sslmode=disable", "postgresql://u:<redacted>@h/db?sslmode=disable"},
		{"text with no credential is unchanged", "installing postgres in namespace postgres", "installing postgres in namespace postgres"},
		{"a URL with no password is unchanged", "postgres://h:5432/db", "postgres://h:5432/db"},
		{"the placeholder is left alone", "postgres://u:<redacted>@h/db", "postgres://u:<redacted>@h/db"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Text(tc.in); got != tc.want {
				t.Errorf("Text(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// root skips a case that needs a permission check to fail, which root passes.
func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root: file permissions do not stop it")
	}
}

func TestTree_ScrubsEveryFileUnderTheDirectory(t *testing.T) {
	RegisterSecret(fakeValue)
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.txt"), "x "+fakeValue+" y")
	write(t, filepath.Join(dir, "deep", "er", "b.json"), `{"u":"postgres://u:`+fakeValue+`@h/db"}`)
	write(t, filepath.Join(dir, "clean.txt"), "nothing here")

	if failed := Tree(dir); len(failed) != 0 {
		t.Fatalf("Tree reported %v", failed)
	}
	for _, f := range []string{"a.txt", "deep/er/b.json"} {
		if got := read(t, filepath.Join(dir, f)); strings.Contains(got, fakeValue) {
			t.Errorf("%s still holds the value: %s", f, got)
		}
	}
	if got := read(t, filepath.Join(dir, "clean.txt")); got != "nothing here" {
		t.Errorf("a clean file changed: %q", got)
	}
}

func TestTree_ReportsAFileItCannotRewrite(t *testing.T) {
	skipIfRoot(t)
	RegisterSecret(fakeValue)
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked.txt")
	write(t, locked, fakeValue)
	if err := os.Chmod(locked, 0o400); err != nil {
		t.Fatal(err)
	}

	failed := Tree(dir)
	if len(failed) != 1 || failed[0] != locked {
		t.Errorf("Tree = %v, want just %s", failed, locked)
	}
}

func TestTree_ReportsADirectoryItCannotRead(t *testing.T) {
	skipIfRoot(t)
	dir := t.TempDir()
	hidden := filepath.Join(dir, "hidden")
	write(t, filepath.Join(hidden, "x.txt"), fakeValue)
	if err := os.Chmod(hidden, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(hidden, 0o700) })

	failed := Tree(dir)
	if len(failed) != 1 || failed[0] != hidden {
		t.Errorf("Tree = %v, want just %s", failed, hidden)
	}
}

// WalkDir does not follow a symlink, the root included. Without resolving it
// first, a symlinked directory was skipped with nothing scrubbed and nothing
// reported.
func TestTree_FollowsASymlinkedRoot(t *testing.T) {
	RegisterSecret(fakeValue)
	target := filepath.Join(t.TempDir(), "real")
	write(t, filepath.Join(target, "sub", "f.txt"), "x "+fakeValue)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}

	if failed := Tree(link); len(failed) != 0 {
		t.Fatalf("Tree reported %v", failed)
	}
	if got := read(t, filepath.Join(target, "sub", "f.txt")); strings.Contains(got, fakeValue) {
		t.Errorf("the file behind the symlink still holds the value: %s", got)
	}
}

func TestTree_ReportsARootThatIsNotADirectory(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file.txt")
	write(t, file, "x")
	missing := filepath.Join(t.TempDir(), "missing")

	for _, root := range []string{file, missing} {
		if failed := Tree(root); len(failed) != 1 || failed[0] != root {
			t.Errorf("Tree(%s) = %v, want just the root", root, failed)
		}
	}
}
