// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"testing"
)

// Strict parsing has to keep accepting what the repo already ships, or the
// lane stops running for a reason that has nothing to do with the lane.
func TestLoadAcceptsShippedConfigs(t *testing.T) {
	paths, err := filepath.Glob("../../*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no candidate config files found; this test is checking nothing")
	}
	for _, p := range paths {
		if _, err := Load(p); err != nil {
			t.Errorf("%s no longer loads: %v", filepath.Base(p), err)
		}
	}
}

// The document forge-site-controller-e2e.yml generates. It is written by a
// heredoc in the workflow rather than from this struct, so nothing but a test
// keeps the two in step.
func TestLoadAcceptsTheGeneratedSiteConfig(t *testing.T) {
	doc := `site:
  jumpHost: "jump.example"
  teleportLogin: "root"
  disableMTLS: true
  images:
    - /w/.site-inputs/controller.tar
    - /w/.site-inputs/capi-provisioned.tar
    - /w/.site-inputs/profiles.tar
  bootstrapEnv:
    NKECTL_CWE_IMAGE: ghcr.io/x/y:v1
    NKECTL_CAPI_PROVISIONED_IMAGE: ghcr.io/x/z:v1
  profilesImage: nke-e2e-profiles:local
`
	if _, err := Load(write(t, doc)); err != nil {
		t.Fatalf("the config the workflow writes no longer loads: %v", err)
	}
}

func TestLoadRejectsAnUnknownKey(t *testing.T) {
	if _, err := Load(write(t, "site:\n  jumpHostt: \"typo\"\n")); err == nil {
		t.Fatal("an unknown key was accepted, so the run would proceed on the default")
	}
}

// The limit of that, recorded because it looks like a gap and is not one.
// sigs.k8s.io/yaml converts to JSON and decodes with encoding/json, whose
// field matching is case-insensitive, so a key differing only in case binds
// to the field rather than counting as unknown. Nothing is dropped, which is
// the property this file is about.
func TestLoadStillAcceptsAKeyDifferingOnlyInCase(t *testing.T) {
	cfg, err := Load(write(t, "site:\n  jumphost: \"jump.example\"\n"))
	if err != nil {
		t.Fatalf("a case variant was rejected: %v", err)
	}
	if cfg.Site.JumpHost != "jump.example" {
		t.Errorf("Site.JumpHost = %q, want it bound from the case variant", cfg.Site.JumpHost)
	}
}

func write(t *testing.T, doc string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "e2e-config.yaml")
	if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
