// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sitespec

import (
	"bytes"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// encodeTemplate renders a parsed template document to bytes, the way Render
// does, so a test can assert on the file order the encoder produces.
func encodeTemplate(t *testing.T, doc *yaml.Node) string {
	t.Helper()
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		t.Fatalf("encode template: %v", err)
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("close encoder: %v", err)
	}
	return buf.String()
}

// mustBefore fails unless a appears before b in out. Both must be present.
func mustBefore(t *testing.T, out, a, b string) {
	t.Helper()
	ai, bi := strings.Index(out, a), strings.Index(out, b)
	if ai < 0 {
		t.Errorf("%q is absent:\n%s", a, out)
		return
	}
	if bi < 0 {
		t.Errorf("%q is absent:\n%s", b, out)
		return
	}
	if ai > bi {
		t.Errorf("%q should appear before %q:\n%s", a, b, out)
	}
}

// TestSetScalarPlacementIsOrderIndependent pins that two optional keys sharing
// one commented-example block land in template order no matter which is written
// first. It calls the low-level setters directly so it does not depend on the
// map-iteration order applySpec used to have.
func TestSetScalarPlacementIsOrderIndependent(t *testing.T) {
	// The two keys' examples both sit in kubeletConfig's head comment, so the
	// order they are written in is what used to decide their file order.
	type write struct{ path, value string }
	for name, order := range map[string][]write{
		"nodeAgentPort first": {
			{pathNodeAgentPort, "50051"},
			{pathControlPlaneVIP, "10.0.0.5"},
		},
		"controlPlaneVIP first": {
			{pathControlPlaneVIP, "10.0.0.5"},
			{pathNodeAgentPort, "50051"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			doc, err := templateDoc()
			if err != nil {
				t.Fatalf("templateDoc: %v", err)
			}
			root := doc.Content[0]
			for _, w := range order {
				if err := setScalar(root, w.path, w.value); err != nil {
					t.Fatalf("setScalar %s: %v", w.path, err)
				}
			}
			out := encodeTemplate(t, doc)

			// File order: controlPlaneVIP, then nodeAgentPort, then kubeletConfig.
			mustBefore(t, out, "controlPlaneVIP:", "nodeAgentPort:")
			mustBefore(t, out, "controlPlaneVIP:", "kubeletConfig:")
			mustBefore(t, out, "nodeAgentPort:", "kubeletConfig:")

			// Each example's prose stays with its own key. The VIP intro sits
			// between serviceCidr and controlPlaneVIP; the port intro sits
			// between controlPlaneVIP and nodeAgentPort.
			mustBefore(t, out, "serviceCidr:", "IPv4 API endpoint advertised by kube-vip")
			mustBefore(t, out, "IPv4 API endpoint advertised by kube-vip", "controlPlaneVIP:")
			mustBefore(t, out, "controlPlaneVIP:", "gRPC port nvke-agent listens on")
			mustBefore(t, out, "gRPC port nvke-agent listens on", "nodeAgentPort:")

			// The commented-out examples are replaced, not duplicated.
			for _, key := range []string{"controlPlaneVIP", "nodeAgentPort"} {
				if strings.Contains(out, "# "+key+":") {
					t.Errorf("the commented example of %s survived alongside the real value:\n%s", key, out)
				}
			}
		})
	}
}
