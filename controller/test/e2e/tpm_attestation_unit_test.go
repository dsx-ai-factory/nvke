// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func tpmTestMachine(name, path string) unstructured.Unstructured {
	m := unstructured.Unstructured{}
	m.SetName(name)
	if path != "" {
		m.SetAnnotations(map[string]string{spireAgentIDPathAnnotation: path})
	}
	return m
}

func TestTPMAgentIDsBuildsOneIDPerMachine(t *testing.T) {
	ids, err := tpmAgentIDs("site.example", []unstructured.Unstructured{
		tpmTestMachine("w-0", "/spire/agent/tpm/bbb"),
		tpmTestMachine("cp-0", "/spire/agent/tpm/aaa"),
	}, 2)
	if err != nil {
		t.Fatalf("tpmAgentIDs() error = %v", err)
	}
	want := []string{"spiffe://site.example/spire/agent/tpm/aaa", "spiffe://site.example/spire/agent/tpm/bbb"}
	if !slices.Equal(ids, want) {
		t.Errorf("ids = %q, want %q", ids, want)
	}
}

func TestTPMAgentIDsRejectsAMachineWithoutATPMIdentity(t *testing.T) {
	// Each of these is a node that joined without its TPM proving who it is:
	// no annotation yet, the bare prefix the controller writes while it waits
	// for CAPNICo's hash, and an identity from another attestor.
	for _, path := range []string{"", "/spire/agent/tpm/", "/spire/agent/join_token/abc", "/spire/agent/dsx-imds/acme/bm/dpu-1"} {
		_, err := tpmAgentIDs("site.example", []unstructured.Unstructured{
			tpmTestMachine("cp-0", "/spire/agent/tpm/aaa"),
			tpmTestMachine("w-0", path),
		}, 2)
		if err == nil || !strings.Contains(err.Error(), "w-0") {
			t.Errorf("path %q: error = %v, want one naming w-0", path, err)
		}
	}
}

func TestTPMAgentIDsRequiresAMachinePerNode(t *testing.T) {
	// A node with no Machine is one this spec would otherwise never check.
	_, err := tpmAgentIDs("site.example", []unstructured.Unstructured{
		tpmTestMachine("cp-0", "/spire/agent/tpm/aaa"),
	}, 2)
	if err == nil || !strings.Contains(err.Error(), "1 CAPI Machines") {
		t.Errorf("error = %v, want one reporting the short count", err)
	}
}

func TestTPMAgentIDsRejectsASharedIdentity(t *testing.T) {
	// One attested agent would otherwise pass the SPIRE check for both nodes.
	_, err := tpmAgentIDs("site.example", []unstructured.Unstructured{
		tpmTestMachine("cp-0", "/spire/agent/tpm/aaa"),
		tpmTestMachine("w-0", "/spire/agent/tpm/aaa"),
	}, 2)
	if err == nil || !strings.Contains(err.Error(), "same TPM identity") {
		t.Errorf("error = %v, want one reporting the shared identity", err)
	}
}

func TestTPMAgentIDsNeedsATrustDomain(t *testing.T) {
	if _, err := tpmAgentIDs("", []unstructured.Unstructured{tpmTestMachine("cp-0", "/spire/agent/tpm/aaa")}, 1); err == nil {
		t.Error("an empty trust domain should be an error, not spiffe:///spire/...")
	}
}
