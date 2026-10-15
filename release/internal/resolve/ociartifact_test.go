// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package resolve

import (
	"strings"
	"testing"
)

func TestOCIArtifactNamesTheArtifact(t *testing.T) {
	for _, tc := range []struct {
		name       string
		base       string
		repository string
		os, arch   string
		version    string
		want       string
	}{
		{
			name: "one tag for every platform", base: "oci://nvcr.io/org",
			repository: "nvke-controller", version: "v0.2.0-rc.1",
			want: "oci://nvcr.io/org/nvke-controller:v0.2.0-rc.1",
		},
		{
			name: "a trailing slash does not double", base: "oci://nvcr.io/org/",
			repository: "nvke-controller", version: "v0.2.0-rc.1",
			want: "oci://nvcr.io/org/nvke-controller:v0.2.0-rc.1",
		},
		{
			// An artifact with no index is one tag per platform, so the
			// architecture is part of the tag rather than part of the pull.
			name: "an architecture puts the platform in the tag", base: "oci://nvcr.io/org",
			repository: "nvkectl", os: "linux", arch: "amd64", version: "v0.2.0-rc.1",
			want: "oci://nvcr.io/org/nvkectl:v0.2.0-rc.1-linux-amd64",
		},
		{
			name: "the described version names it the same way", base: "oci://nvcr.io/org",
			repository: "nvkectl", os: "linux", arch: "arm64", version: "v0.2.0-rc.1-7-g21afa494",
			want: "oci://nvcr.io/org/nvkectl:v0.2.0-rc.1-7-g21afa494-linux-arm64",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := OCIArtifact{
				Base: tc.base, Repository: tc.repository, OS: tc.os, Arch: tc.arch,
			}.Resolve(map[string]string{InputVersionKey: tc.version})
			if err != nil {
				t.Fatal(err)
			}
			if got[ResultURLKey] != tc.want {
				t.Errorf("url = %q, want %q", got[ResultURLKey], tc.want)
			}
			// A location names where a version is. The version half says what
			// that version was.
			if v, ok := got[ResultVersionKey]; ok {
				t.Errorf("a location names no version, got %q", v)
			}
		})
	}
}

func TestOCIArtifactRejects(t *testing.T) {
	a := OCIArtifact{Base: "oci://nvcr.io/org", Repository: "nvkectl", OS: "linux", Arch: "amd64"}

	t.Run("no version to name an artifact for", func(t *testing.T) {
		_, err := a.Resolve(nil)
		if err == nil || !strings.Contains(err.Error(), "no version given") {
			t.Fatalf("err = %v, want it to say there is no version", err)
		}
	})

	t.Run("an override names a location this cannot keep honest", func(t *testing.T) {
		_, err := a.Resolve(map[string]string{
			InputVersionKey:  "v0.1.0",
			InputOverrideKey: "oci://elsewhere/nvkectl:x-linux-amd64",
		})
		if err == nil || !strings.Contains(err.Error(), "override the version instead") {
			t.Fatalf("err = %v, want it to send the override to the version", err)
		}
	})
}

func TestNewOCIArtifact(t *testing.T) {
	with := func(extra map[string]string) map[string]string {
		m := map[string]string{"base": "oci://nvcr.io/org", "repository": "nvkectl"}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}

	// Every artifact a run stages is linux, so an entry naming an architecture
	// and no os gets one rather than losing a field from the tag.
	t.Run("arch alone defaults the os to linux", func(t *testing.T) {
		r, err := NewLocation(LocationStrategyOCIArtifact, with(map[string]string{"arch": "amd64"}), Options{})
		if err != nil {
			t.Fatal(err)
		}
		got, err := r.Resolve(map[string]string{InputVersionKey: "v0.1.0"})
		if err != nil {
			t.Fatal(err)
		}
		if want := "oci://nvcr.io/org/nvkectl:v0.1.0-linux-amd64"; got[ResultURLKey] != want {
			t.Errorf("url = %q, want %q", got[ResultURLKey], want)
		}
	})

	// Without an arch there is no platform suffix at all, which is how a
	// component published as a single tag uses this.
	t.Run("no arch leaves the os unset", func(t *testing.T) {
		r, err := NewLocation(LocationStrategyOCIArtifact, with(nil), Options{})
		if err != nil {
			t.Fatal(err)
		}
		got, err := r.Resolve(map[string]string{InputVersionKey: "v0.1.0"})
		if err != nil {
			t.Fatal(err)
		}
		if want := "oci://nvcr.io/org/nvkectl:v0.1.0"; got[ResultURLKey] != want {
			t.Errorf("url = %q, want %q", got[ResultURLKey], want)
		}
	})

	for _, tc := range []struct {
		name string
		with map[string]string
		want string
	}{
		{"needs a repository", map[string]string{"base": "oci://nvcr.io/org"}, `needs "repository"`},
		{"needs a base", map[string]string{"repository": "nvkectl"}, `needs "base"`},
		{"takes no unknown key", with(map[string]string{"from": "cli"}), "does not take from"},
		{"os without arch names nothing", with(map[string]string{"os": "linux"}), "takes os only with arch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewLocation(LocationStrategyOCIArtifact, tc.with, Options{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}
