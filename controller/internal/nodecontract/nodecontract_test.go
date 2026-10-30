// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodecontract

import (
	"testing"

	"github.com/dsx-ai-factory/nvke/auth/authn"
)

func TestForAgentVersionBoundary(t *testing.T) {
	tests := []struct {
		name    string
		version string
		want    Contracts
	}{
		{"empty is old", "", oldForm()},
		{"whitespace is old", "  ", oldForm()},
		{"unparseable is old", "not-a-version", oldForm()},
		{"far older is old", "v0.1.26", oldForm()},
		{"just below cutover is old", "0.2.99", oldForm()},
		{"prefixed release below cutover is old", "v0.2.0", oldForm()},
		{"cutover is new", "0.3.0", newForm()},
		{"prefixed cutover is new", "v0.3.0", newForm()},
		{"cutover release candidate is new", "v0.3.0-rc.1", newForm()},
		{"cutover git describe is new", "v0.3.0-3-gabc1234", newForm()},
		{"dirty cutover is new", "v0.3.0-dirty", newForm()},
		{"older git describe is old", "v0.2.0-7-gabc1234", oldForm()},
		{"later is new", "v1.0.0", newForm()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ForAgentVersion(tt.version); got != tt.want {
				t.Fatalf("ForAgentVersion(%q) = %+v, want %+v", tt.version, got, tt.want)
			}
		})
	}
}

// Each of the six contracts is checked at the boundary, including the empty
// version, so a field that is not wired to the gate cannot hide behind the others.
// ControllerAudience is the one that does not flip between two spellings: the
// old form carries no audience at all, because an agent that predates the flag
// refuses to start when handed it.
func TestEveryContractFlipsAtTheBoundary(t *testing.T) {
	for _, version := range []string{"", "0.2.9"} {
		got := ForAgentVersion(version)
		if got.URN != "urn:nvidia:nke:controller" ||
			got.UnitName != "nke-agent.service" ||
			got.EnvFilePath != "/etc/default/nke-agent" ||
			got.CNIPackagePrefix != "nke-cni-" ||
			got.ControllerCABundlePath != "/etc/nke/controller-ca.crt" ||
			got.ControllerAudience != "" {
			t.Errorf("version %q: not every contract is the old form: %+v", version, got)
		}
	}
	got := ForAgentVersion(NewFormMinAgentVersion)
	if got.URN != "urn:nvidia:nvke:controller" ||
		got.UnitName != "nvke-agent.service" ||
		got.EnvFilePath != "/etc/default/nvke-agent" ||
		got.CNIPackagePrefix != "nvke-cni-" ||
		got.ControllerCABundlePath != "/etc/nvke/controller-ca.crt" ||
		got.ControllerAudience != authn.NodeAgentAudience {
		t.Errorf("cutover version: not every contract is the new form: %+v", got)
	}
}

// The new-form audience must be the renamed one, not the legacy spelling the
// agent already defaults to: selecting the legacy audience from the controller
// would leave the window in authn.NodeAgentAudienceLegacy open forever.
func TestNewFormSelectsTheRenamedAudience(t *testing.T) {
	if got := ForAgentVersion(NewFormMinAgentVersion).ControllerAudience; got != "nvke-controller" {
		t.Fatalf("new-form ControllerAudience = %q, want %q", got, "nvke-controller")
	}
	if authn.NodeAgentAudience == authn.NodeAgentAudienceLegacy {
		t.Fatal("the renamed and legacy audiences are the same constant; the rename has been undone")
	}
}
