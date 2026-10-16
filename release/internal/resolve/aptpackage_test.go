// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package resolve

import (
	"compress/gzip"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// aptRepo serves an index per suite and architecture, from the package names
// each one holds.
func aptRepo(t *testing.T, gzipped bool, names map[string][]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for key, pkgs := range names {
		suite, arch, _ := strings.Cut(key, "/")
		var body strings.Builder
		for _, p := range pkgs {
			stream := p[strings.LastIndexByte(p, '-')+1:]
			fmt.Fprintf(&body, "Package: %s\nVersion: %s\nArchitecture: %s\n\n", p, stream, arch)
		}
		path := fmt.Sprintf("/nvke-apt-%s/dists/nvke-%s/main/binary-%s/Packages", suite, suite, arch)
		content := body.String()
		if !gzipped {
			mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, content)
			})
			continue
		}
		mux.HandleFunc(path+".gz", func(w http.ResponseWriter, _ *http.Request) {
			zw := gzip.NewWriter(w)
			defer zw.Close()
			fmt.Fprint(zw, content)
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// both returns the two package names a stream is served under.
func both(streams ...string) []string {
	var out []string
	for _, s := range streams {
		out = append(out, "nvke-cni-ovn-images-"+s, "nvke-cni-ovn-charts-"+s)
	}
	return out
}

func newAptPackage(base string) AptPackage {
	return AptPackage{
		Base:     base,
		Suites:   []string{"noble", "resolute"},
		Arches:   []string{"amd64", "arm64"},
		Packages: []string{"nvke-cni-ovn-images", "nvke-cni-ovn-charts"},
	}
}

func everySuite(streams ...string) map[string][]string {
	out := map[string][]string{}
	for _, suite := range []string{"noble", "resolute"} {
		for _, arch := range []string{"amd64", "arm64"} {
			out[suite+"/"+arch] = both(streams...)
		}
	}
	return out
}

func TestAptPackageTakesTheHighestServedEverywhere(t *testing.T) {
	srv := aptRepo(t, false, everySuite("1.4.0", "1.4.11", "1.5.18"))
	got, err := newAptPackage(srv.URL).Resolve(map[string]string{InputConstraintKey: "1.4"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got[ResultVersionKey] != "1.4.11" {
		t.Errorf("version = %q, want 1.4.11", got[ResultVersionKey])
	}
}

func TestAptPackageReadsAGzippedIndex(t *testing.T) {
	srv := aptRepo(t, true, everySuite("1.4.11"))
	got, err := newAptPackage(srv.URL).Resolve(map[string]string{InputConstraintKey: "1.4"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got[ResultVersionKey] != "1.4.11" {
		t.Errorf("version = %q, want 1.4.11", got[ResultVersionKey])
	}
}

// A stream one suite never got is not installable on a run that covers that
// suite, so it is not on offer. This is the 1.4.0 case that reached hardware.
func TestAptPackageSkipsAStreamASuiteDoesNotServe(t *testing.T) {
	names := everySuite("1.4.11")
	names["noble/amd64"] = both("1.4.0", "1.4.11")
	names["noble/arm64"] = both("1.4.0", "1.4.11")
	srv := aptRepo(t, false, names)

	got, err := newAptPackage(srv.URL).Resolve(map[string]string{InputConstraintKey: "1.4"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got[ResultVersionKey] != "1.4.11" {
		t.Errorf("version = %q, want 1.4.11, not the stream only noble serves", got[ResultVersionKey])
	}
}

// Served as charts and not as images is served for neither purpose.
func TestAptPackageRequiresEveryPackage(t *testing.T) {
	names := everySuite("1.4.11")
	names["resolute/arm64"] = []string{"nvke-cni-ovn-charts-1.4.11"}
	srv := aptRepo(t, false, names)

	_, err := newAptPackage(srv.URL).Resolve(map[string]string{InputConstraintKey: "1.4"})
	if err == nil {
		t.Fatal("Resolve succeeded on a stream served as only one of its packages")
	}
}

// A constraint naming every part pins: it matches one version and no other, so
// a plan stays on it when a higher patch is published.
func TestAptPackageConstraintPinsWhenItNamesEveryPart(t *testing.T) {
	srv := aptRepo(t, false, everySuite("1.4.0", "1.4.11", "1.5.18"))
	got, err := newAptPackage(srv.URL).Resolve(map[string]string{InputConstraintKey: "1.4.0"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got[ResultVersionKey] != "1.4.0" {
		t.Errorf("version = %q, want 1.4.0, not the highest on the 1.4 line", got[ResultVersionKey])
	}
}

// A pin on a version no suite serves fails, rather than falling back to one
// that is served. Falling back would qualify something nobody chose.
func TestAptPackagePinNotServedFails(t *testing.T) {
	srv := aptRepo(t, false, everySuite("1.4.11", "1.5.18"))
	_, err := newAptPackage(srv.URL).Resolve(map[string]string{InputConstraintKey: "1.4.12"})
	if err == nil {
		t.Fatal("Resolve succeeded on a pin no suite serves")
	}
	if !strings.Contains(err.Error(), "1.4.12") {
		t.Errorf("error does not name the pin: %v", err)
	}
}

func TestAptPackageRejectsAnOverrideNotServed(t *testing.T) {
	srv := aptRepo(t, false, everySuite("1.4.11"))
	_, err := newAptPackage(srv.URL).Resolve(map[string]string{
		InputConstraintKey: "1.4",
		InputOverrideKey:   "1.4.0",
	})
	if err == nil {
		t.Fatal("Resolve accepted an override no suite serves")
	}
	if !strings.Contains(err.Error(), "1.4.11") {
		t.Errorf("error does not name what is served: %v", err)
	}
}

func TestAptPackageTakesAnOverrideThatIsServed(t *testing.T) {
	srv := aptRepo(t, false, everySuite("1.4.0", "1.4.11"))
	got, err := newAptPackage(srv.URL).Resolve(map[string]string{
		InputConstraintKey: "1.4",
		InputOverrideKey:   "1.4.0",
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got[ResultVersionKey] != "1.4.0" {
		t.Errorf("version = %q, want 1.4.0", got[ResultVersionKey])
	}
}

func TestAptPackageRejectsAnOverrideOutsideTheConstraint(t *testing.T) {
	srv := aptRepo(t, false, everySuite("1.4.11", "1.5.18"))
	_, err := newAptPackage(srv.URL).Resolve(map[string]string{
		InputConstraintKey: "1.4",
		InputOverrideKey:   "1.5.18",
	})
	if err == nil {
		t.Fatal("Resolve accepted an override outside the constraint")
	}
}

func TestAptPackageNeedsAConstraint(t *testing.T) {
	srv := aptRepo(t, false, everySuite("1.4.11"))
	if _, err := newAptPackage(srv.URL).Resolve(map[string]string{}); err == nil {
		t.Fatal("Resolve succeeded with no constraint")
	}
	if err := (AptPackage{}).Validate(map[string]string{}); err == nil {
		t.Fatal("Validate accepted a dependency declaring no constraint")
	}
	if err := (AptPackage{}).Validate(map[string]string{InputConstraintKey: "1.4"}); err != nil {
		t.Fatalf("Validate rejected a constraint: %v", err)
	}
	if err := (AptPackage{}).Validate(map[string]string{InputConstraintKey: "nineteen"}); err == nil {
		t.Fatal("Validate accepted a constraint that is not a version prefix")
	}
	if err := (AptPackage{}).Validate(map[string]string{
		InputConstraintKey: "1.4",
		InputReleasesKey:   "24.04",
	}); err == nil {
		t.Fatal("Validate accepted releases, which it cannot use")
	}
}

// A suite that does not answer is a failure and never an empty set: treating
// it as "serves nothing" would quietly resolve from the suites that did.
func TestAptPackageFailsWhenASuiteCannotBeRead(t *testing.T) {
	names := everySuite("1.4.11")
	delete(names, "resolute/arm64")
	srv := aptRepo(t, false, names)

	_, err := newAptPackage(srv.URL).Resolve(map[string]string{InputConstraintKey: "1.4"})
	if err == nil {
		t.Fatal("Resolve succeeded with a suite that did not answer")
	}
	if !strings.Contains(err.Error(), "resolute") {
		t.Errorf("error does not name the suite: %v", err)
	}
}
