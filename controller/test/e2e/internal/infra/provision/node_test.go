// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package provision

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// The saved key is readable only by its owner, and is the private half of the
// public key the node is given.
func TestKeySave(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "node-keys")

	key, err := NewKey("test")
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Fatalf("nothing was saved, but %s exists (err %v)", dir, statErr)
	}
	err = CheckKeyPath(dir, "site-a")
	if err != nil {
		t.Fatalf("a free path was refused: %v", err)
	}
	err = key.Save(dir, "site-a")
	if err != nil {
		t.Fatal(err)
	}
	signer, authorized := key.Signer, key.Authorized
	path := KeyPath(dir, "site-a")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("key mode = %o, want 600", mode)
	}
	info, err = os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o700 {
		t.Errorf("key dir mode = %o, want 700", mode)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := ssh.ParsePrivateKey(b)
	if err != nil {
		t.Fatalf("parsing the saved key: %v", err)
	}
	if !bytes.Equal(saved.PublicKey().Marshal(), signer.PublicKey().Marshal()) {
		t.Error("the saved key is not the session's key")
	}
	if got := string(bytes.TrimSpace(ssh.MarshalAuthorizedKey(saved.PublicKey()))); got != authorized {
		t.Errorf("saved key's public half = %q, want the authorized key %q", got, authorized)
	}

	// Checked before a machine is made, and saying why, since the key there
	// may still log into a kept node.
	if err := CheckKeyPath(dir, "site-a"); err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "still kept") {
		t.Errorf("CheckKeyPath on a taken path = %v, want the path named and why it is kept", err)
	}
	if err := key.Save(dir, "site-a"); err == nil {
		t.Error("a second key for the same node overwrote the first")
	}
	for _, name := range []string{"", ".", "..", "../x", "a/b"} {
		if err := CheckKeyPath(dir, name); err == nil {
			t.Errorf("node name %q was accepted as a file name", name)
		}
		if err := key.Save(dir, name); err == nil {
			t.Errorf("node name %q was saved as a file name", name)
		}
	}

	if err := RemoveKey(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("%s survived RemoveKey (err %v)", path, err)
	}
	if err := RemoveKey(path); err != nil {
		t.Errorf("removing a key already gone: %v", err)
	}
	if err := RemoveKey(""); err != nil {
		t.Errorf("removing no key: %v", err)
	}
}

// A key directory that already exists is restricted to its owner before a key
// goes in, and a key directory that is not a directory holds no key.
func TestKeySaveExistingDir(t *testing.T) {
	key, err := NewKey("test")
	if err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(t.TempDir(), "node-keys")
	err = os.Mkdir(dir, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	// Mkdir is subject to the umask; make the starting mode the open one.
	err = os.Chmod(dir, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	err = key.Save(dir, "site-a")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o700 {
		t.Errorf("existing key dir mode = %o after Save, want 700", mode)
	}

	notDir := filepath.Join(t.TempDir(), "node-keys")
	if err := os.WriteFile(notDir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := key.Save(notDir, "site-a"); err == nil {
		t.Error("a key was saved under a file")
	}
}

// WithTimeout raises a timeout that is too short and leaves everything else
// alone. Zero is the case worth a test: it means "no timeout", so raising it
// to the floor would be lowering it from infinity, and a caller that left the
// field unset wants its context to be the only bound.
func TestWithTimeout(t *testing.T) {
	floor := 30 * time.Second

	for name, tc := range map[string]struct {
		client *http.Client
		want   time.Duration
	}{
		"nil client takes the floor":  {client: nil, want: floor},
		"absent timeout stays absent": {client: &http.Client{}, want: 0},
		"shorter timeout is raised":   {client: &http.Client{Timeout: time.Second}, want: floor},
		"longer timeout is kept":      {client: &http.Client{Timeout: time.Hour}, want: time.Hour},
		"equal timeout is kept":       {client: &http.Client{Timeout: floor}, want: floor},
	} {
		t.Run(name, func(t *testing.T) {
			if got := WithTimeout(tc.client, floor).Timeout; got != tc.want {
				t.Errorf("Timeout = %v, want %v", got, tc.want)
			}
		})
	}
}

// A copy, so the caller keeps its client and anything it injected into one.
func TestWithTimeoutCopiesRatherThanMutating(t *testing.T) {
	transport := http.DefaultTransport
	original := &http.Client{Timeout: time.Second, Transport: transport}

	raised := WithTimeout(original, time.Minute)

	if original.Timeout != time.Second {
		t.Errorf("the caller's client was mutated: Timeout = %v, want 1s", original.Timeout)
	}
	if raised == original {
		t.Error("WithTimeout returned the caller's client, want a copy")
	}
	if raised.Transport != transport {
		t.Error("the copy lost the injected transport")
	}
}
