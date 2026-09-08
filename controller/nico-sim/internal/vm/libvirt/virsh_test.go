// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package libvirt

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRetry(t *testing.T) {
	// The error libvirt returns when two TPM domains start at once.
	swtpmRace := errors.New("failed to create directory '/run/swtpm': File exists")

	tests := []struct {
		name      string
		failures  int
		cancelled bool
		wantCalls int
		wantErr   error
	}{
		{name: "succeeds first time", failures: 0, wantCalls: 1},
		{name: "second attempt wins the race", failures: 1, wantCalls: 2},
		{name: "every attempt fails", failures: 3, wantCalls: 3, wantErr: swtpmRace},
		{name: "cancelled context stops retrying", failures: 3, cancelled: true, wantCalls: 1, wantErr: context.Canceled},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tt.cancelled {
				cancel()
			}

			calls := 0
			err := retry(ctx, 3, time.Millisecond, func() error {
				calls++
				if calls <= tt.failures {
					return swtpmRace
				}
				return nil
			})

			if calls != tt.wantCalls {
				t.Errorf("called fn %d times, want %d", calls, tt.wantCalls)
			}
			if tt.wantErr == nil && err != nil {
				t.Fatalf("retry returned %v, want nil", err)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("retry returned %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// A wrong guess at libvirt's swtpm state directory means the EK cert we
// captured belongs to a TPM the guest never booted with, and TPM attestation
// cannot succeed. Catch it here rather than in a SPIRE x509 error.
func TestSwtpmStateMismatch(t *testing.T) {
	const uuid = "7e720345-740c-4d62-a258-e9d3193826cf"
	dumpxml := "<domain><uuid>" + uuid + "</uuid></domain>"

	tests := []struct {
		name     string
		seeded   bool // state exists where we put it
		other    bool // state exists where libvirt actually looked
		wantMism bool
	}{
		{name: "libvirt used our state", seeded: true, wantMism: false},
		{name: "libvirt made its own elsewhere", other: true, wantMism: true},
		{name: "both present, ours wins", seeded: true, other: true, wantMism: false},
		{name: "neither present is not evidence", wantMism: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Point both candidates inside the test's own tree.
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			systemSwtpmRoot = t.TempDir()
			t.Cleanup(func() { systemSwtpmRoot = "/var/lib/libvirt/swtpm" })
			r := &virsh{uri: "qemu:///session"}

			if tc.seeded {
				mkStateDir(t, r.swtpmStatePath(uuid))
			}
			if tc.other {
				mkStateDir(t, systemSwtpmStatePath(uuid))
			}

			used, gotUUID, mismatch := r.swtpmStateMismatch(dumpxml)
			if gotUUID != uuid {
				t.Errorf("uuid = %q, want %q", gotUUID, uuid)
			}
			if mismatch != tc.wantMism {
				t.Errorf("mismatch = %v, want %v (used %q)", mismatch, tc.wantMism, used)
			}
		})
	}

	t.Run("no uuid in xml", func(t *testing.T) {
		r := &virsh{uri: "qemu:///session"}
		if _, _, mismatch := r.swtpmStateMismatch("<domain/>"); mismatch {
			t.Error("missing uuid must not report a mismatch")
		}
	})
}

// mkStateDir creates dir with one file in it, as swtpm leaves it.
func mkStateDir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tpm2-00.permall"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Both candidate directories must end up with the same EK, so the cert is
// right whichever one libvirt picks.
func TestMirrorSwtpmState(t *testing.T) {
	const uuid = "7e720345-740c-4d62-a258-e9d3193826cf"
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	systemSwtpmRoot = t.TempDir()
	t.Cleanup(func() { systemSwtpmRoot = "/var/lib/libvirt/swtpm" })

	r := &virsh{uri: "qemu:///session"}
	seeded := r.swtpmStatePath(uuid)
	mkStateDir(t, seeded)

	if err := r.mirrorSwtpmState(seeded, uuid); err != nil {
		t.Fatal(err)
	}

	mirrored := filepath.Join(systemSwtpmStatePath(uuid), "tpm2-00.permall")
	got, err := os.ReadFile(mirrored)
	if err != nil {
		t.Fatalf("state not mirrored to %s: %v", mirrored, err)
	}
	if string(got) != "x" {
		t.Errorf("mirrored state = %q, want the bytes of the original", got)
	}
	if _, _, mismatch := r.swtpmStateMismatch("<domain><uuid>" + uuid + "</uuid></domain>"); mismatch {
		t.Error("mirrored state must not read as a mismatch")
	}
}
