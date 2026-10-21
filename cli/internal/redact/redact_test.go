// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package redact

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// A short secret that is a substring of a long one must not split it, whatever
// the order they were registered in.
func TestRegisterSecretOrderDoesNotMatter(t *testing.T) {
	for i := 0; i < 20; i++ {
		long := fmt.Sprintf("Zq%dLongToken9vX", i)
		short := fmt.Sprintf("Zq%dL", i)
		first, second := short, long
		if i%2 == 0 {
			first, second = long, short
		}
		RegisterSecret(first)
		RegisterSecret(second)
		if got := Text("a " + long + " b"); got != "a "+Placeholder+" b" {
			t.Fatalf("round %d: %q", i, got)
		}
	}
}

// ReplaceAttr leaves slog's own time, level and source alone, and masks the
// message and user attributes, in groups too.
func TestReplaceAttrLeavesBuiltInKeysAlone(t *testing.T) {
	const secret = "builtin-secret-1234"
	RegisterSecret(secret)
	for _, attr := range []slog.Attr{
		slog.Time(slog.TimeKey, time.Now()),
		slog.Any(slog.LevelKey, slog.LevelInfo),
		slog.Any(slog.SourceKey, &slog.Source{Function: "f", File: "f.go", Line: 1}),
	} {
		if got := ReplaceAttr(nil, attr); !got.Equal(attr) {
			t.Errorf("ReplaceAttr changed the built-in %s: %v", attr.Key, got)
		}
	}
	for name, got := range map[string]slog.Attr{
		"a user attribute named time":   ReplaceAttr(nil, slog.String(slog.TimeKey, secret)),
		"a user attribute named level":  ReplaceAttr(nil, slog.String(slog.LevelKey, secret)),
		"a user attribute named source": ReplaceAttr(nil, slog.String(slog.SourceKey, secret)),
		"the message":                   ReplaceAttr(nil, slog.String(slog.MessageKey, secret)),
		"an attribute in a group":       ReplaceAttr([]string{"g"}, slog.String(slog.LevelKey, secret)),
		"an attribute":                  ReplaceAttr(nil, slog.String("attr", secret)),
		"an attribute of a group":       ReplaceAttr(nil, slog.Group("g", slog.String("inner", secret))),
	} {
		if text := got.Value.String(); strings.Contains(text, secret) {
			t.Errorf("%s is not masked: %q", name, text)
		}
	}
}
