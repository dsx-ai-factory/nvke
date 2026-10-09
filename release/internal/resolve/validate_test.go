// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package resolve

import (
	"strings"
	"testing"
)

// Each strategy says what it takes and needs among the inputs a subject can
// declare, so the code that reads a subject does not have to.
func TestValidate(t *testing.T) {
	image := OSImage{Base: "https://cdn.example", From: "os-image", Arch: "amd64", Variant: "non-fde"}

	for _, tc := range []struct {
		name   string
		r      Resolver
		inputs map[string]string
		want   string
	}{
		{"gitTag takes nothing", GitTag{}, nil, ""},
		{"gitTag rejects a constraint", GitTag{}, map[string]string{InputConstraintKey: "1"}, "takes no constraint"},
		{"makeVar takes nothing", MakeVar{}, nil, ""},
		{"makeVar rejects a value", MakeVar{}, map[string]string{InputValueKey: "x"}, "takes no value"},

		{"lockedPackage takes a constraint", LockedPackage{}, map[string]string{InputConstraintKey: "1.34"}, ""},
		{"lockedPackage needs one", LockedPackage{}, nil, "needs constraint"},
		{"lockedPackage wants a version prefix", LockedPackage{}, map[string]string{InputConstraintKey: "latest"}, "is not a version prefix"},
		{"lockedPackage rejects releases", LockedPackage{}, map[string]string{InputConstraintKey: "1.34", InputReleasesKey: "1.34.2"}, "takes no releases"},

		{"osImage takes a series and releases", image, map[string]string{InputConstraintKey: "24.04", InputReleasesKey: "24.04.5"}, ""},
		{"osImage needs releases", image, map[string]string{InputConstraintKey: "24.04"}, "needs releases"},
		{"osImage needs a series", image, map[string]string{InputReleasesKey: "24.04.5"}, "needs constraint"},
		{"osImage wants a release in the series", image, map[string]string{InputConstraintKey: "26.04", InputReleasesKey: "24.04.5"}, "none of the releases 24.04.5 matches Ubuntu 26.04"},
		{"osImage rejects a value", image, map[string]string{InputConstraintKey: "24.04", InputReleasesKey: "24.04.5", InputValueKey: "x"}, "takes no value"},

		{"url under a base takes a path", URL{Base: "https://cdn.example"}, map[string]string{InputValueKey: "efi/x.efi"}, ""},
		{"url under a base rejects a whole URL", URL{Base: "https://cdn.example"}, map[string]string{InputValueKey: "https://other.example/x"}, "is a whole URL"},
		{"url with no base takes a whole URL", URL{}, map[string]string{InputValueKey: "https://cdn.example/x"}, ""},
		{"url with no base rejects a path", URL{}, map[string]string{InputValueKey: "efi/x.efi"}, "is not a URL"},
		{"url needs a value", URL{}, nil, "needs value"},
		{"url rejects a constraint", URL{}, map[string]string{InputValueKey: "https://cdn.example/x", InputConstraintKey: "1"}, "takes no constraint"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.r, tc.inputs)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// A resolver that does not validate accepts whatever it is declared.
func TestValidateAcceptsAResolverThatDoesNotValidate(t *testing.T) {
	if err := Validate(plain{}, map[string]string{InputValueKey: "x"}); err != nil {
		t.Fatal(err)
	}
}

type plain struct{}

func (plain) Resolve(map[string]string) (map[string]string, error) { return nil, nil }
