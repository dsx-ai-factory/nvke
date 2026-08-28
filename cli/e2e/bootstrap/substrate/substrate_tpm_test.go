// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package substrate

import (
	"path/filepath"
	"testing"
)

func TestIsTPMServerRefusal(t *testing.T) {
	for name, tc := range map[string]struct {
		line string
		want bool
	}{
		"the busy TPM that flaked the suite": {
			line: `level=warning msg="Failed to retrieve attestation result" error="rpc error: code = Internal desc = nodeattestor(tpm): failed to generate attestation data: failed to connect to tpm: open /dev/tpm0: device or resource busy" retry_interval=6.759494953s`,
			want: false,
		},
		"a real refusal from the server": {
			line: `level=error msg="Failed to retrieve attestation result" error="rpc error: code = PermissionDenied desc = nodeattestor(tpm): could not validate EK: hash not in allowlist"`,
			want: true,
		},
		"no such line at all": {line: "", want: false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := isTPMServerRefusal(tc.line); got != tc.want {
				t.Errorf("isTPMServerRefusal(%q) = %v, want %v", tc.line, got, tc.want)
			}
		})
	}
}

func TestParseJournalCursor(t *testing.T) {
	cursor, err := parseJournalCursor("one journal line\n-- cursor: s=abc;i=42;b=def\n")
	if err != nil {
		t.Fatal(err)
	}
	if cursor != "s=abc;i=42;b=def" {
		t.Fatalf("cursor = %q, want %q", cursor, "s=abc;i=42;b=def")
	}

	if _, err := parseJournalCursor("one journal line\n"); err == nil {
		t.Fatal("parseJournalCursor accepted output without a cursor")
	}
}

func TestAttestationBaselineRoundTrip(t *testing.T) {
	want := AttestationBaseline{
		Version: attestationBaselineVersion,
		Nodes: []AttestationNodeBaseline{
			{Name: "node1", EKPubhash: "aaa", JournalCursor: "cursor-1"},
			{Name: "node2", EKPubhash: "bbb", JournalCursor: "cursor-2"},
		},
	}
	path := filepath.Join(t.TempDir(), "nested", "baseline.json")
	if err := WriteAttestationBaseline(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadAttestationBaseline(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Nodes) != len(want.Nodes) || got.Nodes[0] != want.Nodes[0] || got.Nodes[1] != want.Nodes[1] {
		t.Fatalf("loaded baseline = %#v, want %#v", got, want)
	}
}

func TestValidateAttestationBaselineRejectsChangedEK(t *testing.T) {
	s := &Substrate{nodes: []Node{{Name: "node1", EKPubhash: "current"}}}
	err := s.validateAttestationBaseline(AttestationBaseline{
		Version: attestationBaselineVersion,
		Nodes: []AttestationNodeBaseline{{
			Name:          "node1",
			EKPubhash:     "stale",
			JournalCursor: "cursor",
		}},
	})
	if err == nil {
		t.Fatal("validateAttestationBaseline accepted a stale EK fingerprint")
	}
}
