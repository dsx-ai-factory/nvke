// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package resolve

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// server answers 200 for the paths it is given and 403 for the rest, which is
// what the real bucket does for an object that is not there.
func server(t *testing.T, present ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, p := range present {
			if r.URL.Path == p {
				return
			}
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// everything answers 200 to any path.
func everything(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(srv.Close)
	return srv
}

func newOSImage(base, arch string) OSImage {
	return OSImage{Base: base, From: "os-image", Arch: arch, Variant: "non-fde"}
}

// imageInputs are what the planner gives an image: the release tag, the series
// of the stream and the point releases the subject declares.
func imageInputs() map[string]string {
	return map[string]string{
		"os-image":         "v0.1.1-rc.1",
		InputConstraintKey: "24.04",
		InputReleasesKey:   "24.04.5",
	}
}

func TestOSImageBuildsTheImageURL(t *testing.T) {
	srv := everything(t)

	for _, tc := range []string{"amd64", "arm64"} {
		t.Run(tc, func(t *testing.T) {
			// A trailing slash on the base does not double up.
			got, err := newOSImage(srv.URL+"/nke-packages/", tc).Resolve(imageInputs())
			if err != nil {
				t.Fatal(err)
			}
			want := srv.URL + "/nke-packages/os-image/releases/v0.1.1-rc.1/" + tc + "/nvke-node-non-fde-ubuntu-24.04.5-" + tc + ".qcow2"
			if got[ResultURLKey] != want {
				t.Errorf("url = %q, want %q", got[ResultURLKey], want)
			}
			if got[ResultVersionKey] != "v0.1.1-rc.1" {
				t.Errorf("version = %q, want the release tag", got[ResultVersionKey])
			}
		})
	}
}

// The series a stream declares picks the point release from the list.
func TestOSImagePicksThePointRelease(t *testing.T) {
	srv := everything(t)

	for _, tc := range []struct{ name, series, releases, want string }{
		{"the only one", "24.04", "24.04.5", "24.04.5"},
		{"the highest of a series, numerically", "24.04", "24.04.5,24.04.10,26.04.1", "24.04.10"},
		{"another series", "26.04", "24.04.5,26.04.1", "26.04.1"},
		{"a series is not a prefix of a longer number", "24.04", "24.045.1,24.04.2", "24.04.2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := imageInputs()
			in[InputConstraintKey], in[InputReleasesKey] = tc.series, tc.releases
			got, err := newOSImage(srv.URL, "amd64").Resolve(in)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(got[ResultURLKey], "ubuntu-"+tc.want+"-amd64.qcow2") {
				t.Errorf("url = %q, want point release %s", got[ResultURLKey], tc.want)
			}
		})
	}
}

func TestOSImageRejectsMissingInputs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(map[string]string)
		want   string
	}{
		{"no release tag", func(m map[string]string) { delete(m, "os-image") }, "no os-image given"},
		{"no series", func(m map[string]string) { delete(m, InputConstraintKey) }, "no constraint given"},
		{"no releases", func(m map[string]string) { delete(m, InputReleasesKey) }, "no releases given"},
		{"no release in the series", func(m map[string]string) { m[InputConstraintKey] = "26.04" }, "none of the releases 24.04.5 matches Ubuntu 26.04"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := imageInputs()
			tc.change(in)
			_, err := newOSImage("https://cdn.example/p", "amd64").Resolve(in)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// A URL a person gave replaces the built one and is checked like it. The image's
// release is then not known, so there is no version.
func TestOSImageOverride(t *testing.T) {
	srv := everything(t)
	in := map[string]string{InputOverrideKey: srv.URL + "/custom.qcow2"}
	got, err := newOSImage("https://cdn.example/p", "amd64").Resolve(in)
	if err != nil {
		t.Fatal(err)
	}
	if got[ResultURLKey] != srv.URL+"/custom.qcow2" {
		t.Errorf("url = %q", got[ResultURLKey])
	}
	if v, ok := got[ResultVersionKey]; ok {
		t.Errorf("an override has no release to name, got version %q", v)
	}

	in[InputOverrideKey] = "v0.1.1-rc.1"
	_, err = newOSImage("https://cdn.example/p", "amd64").Resolve(in)
	if err == nil || !strings.Contains(err.Error(), `"v0.1.1-rc.1" is not a URL`) {
		t.Errorf("err = %v, want it to say a version is not a URL", err)
	}
}

func TestNewOSImageAndURL(t *testing.T) {
	with := map[string]string{"base": "https://cdn.example/p", "from": "os-image", "arch": "amd64", "variant": "non-fde"}
	if _, err := New(StrategyOSImage, with, "."); err != nil {
		t.Fatalf("a complete configuration should build: %v", err)
	}
	if _, err := New(StrategyURL, nil, "."); err != nil {
		t.Fatalf("url takes no configuration: %v", err)
	}

	for _, tc := range []struct {
		name     string
		strategy string
		change   func(map[string]string)
		want     string
	}{
		{"a missing key", StrategyOSImage, func(m map[string]string) { delete(m, "arch") }, `needs "arch"`},
		{"a key it does not take", StrategyOSImage, func(m map[string]string) { m["extra"] = "x" }, "does not take extra"},
		{"url takes only a base", StrategyURL, func(m map[string]string) { m["path"] = "x" }, "does not take path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := map[string]string{}
			if tc.strategy == StrategyOSImage {
				for k, v := range with {
					bad[k] = v
				}
			}
			tc.change(bad)
			_, err := New(tc.strategy, bad, ".")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// A release published before the rename keeps its original filename. When the
// new name is not there, Resolve returns that one.
func TestOSImageLegacyFilename(t *testing.T) {
	legacy := newOSImage("", "amd64").imageObjectURL("", "v0.1.1-rc.1", "24.04.5", "nke")
	srv := server(t, legacy)

	got, err := newOSImage(srv.URL, "amd64").Resolve(imageInputs())
	if err != nil {
		t.Fatal(err)
	}
	if got[ResultURLKey] != srv.URL+legacy {
		t.Errorf("url = %q, want the filename published before the rename", got[ResultURLKey])
	}
	if got[ResultVersionKey] != "v0.1.1-rc.1" {
		t.Errorf("version = %q, want the release tag", got[ResultVersionKey])
	}
}

// The URL is always asked for, and a missing one fails with both explanations of
// a 403.
func TestOSImageChecksTheURLExists(t *testing.T) {
	const image = "/os-image/releases/v0.1.1-rc.1/amd64/nvke-node-non-fde-ubuntu-24.04.5-amd64.qcow2"
	srv := server(t, image)

	t.Run("it exists", func(t *testing.T) {
		if _, err := newOSImage(srv.URL, "amd64").Resolve(imageInputs()); err != nil {
			t.Fatalf("the URL answers 200: %v", err)
		}
	})

	t.Run("it is not there, and the error names both explanations", func(t *testing.T) {
		in := imageInputs()
		in[InputConstraintKey], in[InputReleasesKey] = "26.04", "26.04.1"
		_, err := newOSImage(srv.URL, "amd64").Resolve(in)
		if err == nil {
			t.Fatal("expected an error, got none")
		}
		for _, want := range []string{"could not confirm", "nvke-node-non-fde-ubuntu-26.04.1-amd64.qcow2", "HTTP 403", "a network that is not allowed both answer 403"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error does not mention %q:\n%s", want, err)
			}
		}
	})

	t.Run("an unreachable host is reported", func(t *testing.T) {
		_, err := newOSImage("http://127.0.0.1:1", "amd64").Resolve(imageInputs())
		if err == nil || !strings.Contains(err.Error(), "could not confirm") {
			t.Errorf("err = %v, want it to say it could not confirm", err)
		}
	})
}

func TestURL(t *testing.T) {
	srv := server(t, "/efi/qcow-imager.efi", "/other.efi")

	for _, tc := range []struct {
		name   string
		inputs map[string]string
		want   string
		wantEr string
	}{
		{"the value is the URL", map[string]string{InputValueKey: srv.URL + "/efi/qcow-imager.efi"}, srv.URL + "/efi/qcow-imager.efi", ""},
		{"an override replaces it", map[string]string{InputValueKey: "https://a.example/x", InputOverrideKey: srv.URL + "/other.efi"}, srv.URL + "/other.efi", ""},
		{"nothing given", map[string]string{}, "", "no value given"},
		{"not a URL", map[string]string{InputValueKey: "ftp://host/x"}, "", `"ftp://host/x" is not a URL`},
		{"absent", map[string]string{InputValueKey: srv.URL + "/efi/gone.efi"}, "", "HTTP 403"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := URL{}.Resolve(tc.inputs)
			if tc.wantEr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantEr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantEr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got[ResultURLKey] != tc.want {
				t.Errorf("url = %q, want %q", got[ResultURLKey], tc.want)
			}
			if v, ok := got[ResultVersionKey]; ok {
				t.Errorf("a location has no version, got %q", v)
			}
		})
	}
}

// A path under a base is how a declared URL names where it lives without
// repeating the host, and a whole URL given in its place is refused.
func TestURLUnderABase(t *testing.T) {
	srv := everything(t)
	u := URL{Base: srv.URL + "/"}

	for _, tc := range []struct {
		name   string
		inputs map[string]string
		want   string
		wantEr string
	}{
		{"a path is joined to the base", map[string]string{InputValueKey: "nke-isos/efi/x86_64/qcow-imager.efi"}, srv.URL + "/nke-isos/efi/x86_64/qcow-imager.efi", ""},
		{"a leading slash does not double up", map[string]string{InputValueKey: "/nke-isos/x.efi"}, srv.URL + "/nke-isos/x.efi", ""},
		{"an override is always a whole URL", map[string]string{InputValueKey: "nke-isos/x.efi", InputOverrideKey: srv.URL + "/mine/y.efi"}, srv.URL + "/mine/y.efi", ""},
		{"a whole URL where a path is expected", map[string]string{InputValueKey: "https://other.example/x.efi"}, "", `is a whole URL, but this entry takes a path under ` + srv.URL},
		{"an override that is a path", map[string]string{InputValueKey: "nke-isos/x.efi", InputOverrideKey: "nke-isos/y.efi"}, "", `"nke-isos/y.efi" is not a URL`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := u.Resolve(tc.inputs)
			if tc.wantEr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantEr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantEr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got[ResultURLKey] != tc.want {
				t.Errorf("url = %q, want %q", got[ResultURLKey], tc.want)
			}
		})
	}
}

// The directory the release directories sit in is part of what the image is
// published under, and it may be empty.
func TestOSImagePath(t *testing.T) {
	srv := everything(t)

	for _, tc := range []struct{ name, path, want string }{
		{"with a path", "nke-packages", "/nke-packages/os-image/releases/v0.1.1-rc.1/amd64/nvke-node-non-fde-ubuntu-24.04.5-amd64.qcow2"},
		{"with slashes around it", "/nke-packages/", "/nke-packages/os-image/releases/v0.1.1-rc.1/amd64/nvke-node-non-fde-ubuntu-24.04.5-amd64.qcow2"},
		{"with none", "", "/os-image/releases/v0.1.1-rc.1/amd64/nvke-node-non-fde-ubuntu-24.04.5-amd64.qcow2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := newOSImage(srv.URL, "amd64")
			o.Path = tc.path
			got, err := o.Resolve(imageInputs())
			if err != nil {
				t.Fatal(err)
			}
			if got[ResultURLKey] != srv.URL+tc.want {
				t.Errorf("url = %q, want %q", got[ResultURLKey], srv.URL+tc.want)
			}
		})
	}
}

func TestHighestMatching(t *testing.T) {
	for _, tc := range []struct {
		name       string
		candidates []string
		constraint string
		want       string
		wantOK     bool
	}{
		{"one", []string{"24.04.5"}, "24.04", "24.04.5", true},
		{"numeric, not lexical", []string{"24.04.5", "24.04.10", "24.04.9"}, "24.04", "24.04.10", true},
		{"by parts, not by characters", []string{"24.045.1", "24.04.2"}, "24.04", "24.04.2", true},
		{"none match", []string{"24.04.5"}, "26.04", "", false},
		{"a part that is not a number is never chosen", []string{"24.04.x", "24.04.1"}, "24.04", "24.04.1", true},
		{"nothing to choose from", nil, "24.04", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := HighestMatching(tc.candidates, tc.constraint)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("HighestMatching(%v, %q) = %q, %v, want %q, %v", tc.candidates, tc.constraint, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// A strategy that reads its versions from a source does not take a list.
func TestLockedPackageRejectsReleases(t *testing.T) {
	_, err := LockedPackage{LockDir: "x/lock", From: "os-image"}.Resolve(map[string]string{
		InputConstraintKey: "1.36", InputReleasesKey: "1.36.3",
	})
	if err == nil || !strings.Contains(err.Error(), "takes no releases") {
		t.Errorf("err = %v, want it to say it takes no releases", err)
	}
}
