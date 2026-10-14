// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"bytes"
	"strings"
	"testing"
)

func TestLoad_PinsTheThreePulledCharts(t *testing.T) {
	m := Load()
	for _, c := range []string{ComponentTemporal, ComponentSPIRECRDs, ComponentSPIRE} {
		comp, ok := m.Components[c]
		if !ok || comp.Chart == nil {
			t.Fatalf("component %q has no chart", c)
		}
		if comp.Chart.Name == "" || comp.Chart.Repo == "" || comp.Chart.Version == "" || comp.Chart.SHA256 == "" {
			t.Errorf("component %q chart is incomplete: %+v", c, *comp.Chart)
		}
	}
	for _, c := range []string{ComponentPostgres, ComponentController, ComponentProvisioned, ComponentCAPNICo} {
		if comp := m.Components[c]; comp.Chart != nil {
			t.Errorf("component %q lists a chart; it ships inside nvkectl", c)
		}
	}
}

func TestBytes_IsTheFileAsShipped(t *testing.T) {
	b := Bytes()
	if !bytes.HasPrefix(b, []byte("# SPDX-FileCopyrightText")) || !bytes.Contains(b, []byte("\ncomponents:\n")) {
		t.Errorf("Bytes() is not the embedded file:\n%s", b)
	}
	b[0] = 'X'
	if Bytes()[0] != '#' {
		t.Error("Bytes() returned the embedded slice itself, not a copy")
	}
}

func TestLoad_ListsTheCAPNICoImageWithItsDigest(t *testing.T) {
	img, ok := Load().Components[ComponentCAPNICo].Images[ImageCAPNICo]
	if !ok {
		t.Fatalf("component %q lists no %q image", ComponentCAPNICo, ImageCAPNICo)
	}
	if !strings.Contains(img.Ref, "/") || !strings.Contains(img.Ref, ":") {
		t.Errorf("CAPNICo image ref %q is not a registry reference with a tag", img.Ref)
	}
	if !strings.HasPrefix(img.Digest, "sha256:") || len(img.Digest) != len("sha256:")+64 {
		t.Errorf("CAPNICo image digest %q is not a sha256 digest", img.Digest)
	}
}
