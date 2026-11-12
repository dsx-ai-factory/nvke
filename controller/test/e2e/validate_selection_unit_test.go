// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"testing"

	"github.com/onsi/ginkgo/v2/types"
	"github.com/stretchr/testify/require"
)

func lookupIn(env map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := env[key]
		return value, ok
	}
}

func TestUnit_ValidationLabelFilter(t *testing.T) {
	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"neither set leaves the filter alone": {nil, ""},
		"the site":                            {map[string]string{siteValidateEnv: "true", tenantValidateEnv: "false"}, "site-validate"},
		"the site, tenant unset":              {map[string]string{siteValidateEnv: "true"}, "site-validate"},
		"the site, tenant blank":              {map[string]string{siteValidateEnv: "true", tenantValidateEnv: ""}, "site-validate"},
		"the site and a tenant":               {map[string]string{siteValidateEnv: "true", tenantValidateEnv: "true"}, "site-validate || tenant-validate"},
		"a tenant alone":                      {map[string]string{siteValidateEnv: "false", tenantValidateEnv: "true"}, "tenant-validate"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := validationLabelFilter(lookupIn(tc.env))
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestUnit_ValidationLabelFilterRejects(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"both false":     {siteValidateEnv: "false", tenantValidateEnv: "false"},
		"both blank":     {siteValidateEnv: "", tenantValidateEnv: " "},
		"site blank":     {siteValidateEnv: ""},
		"site garbage":   {siteValidateEnv: "yes please"},
		"tenant garbage": {siteValidateEnv: "true", tenantValidateEnv: "sometimes"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := validationLabelFilter(lookupIn(env))
			require.Error(t, err)
		})
	}
}

// The site scan's pods are privileged and share the host's PID namespace,
// which the validation image's permissions rule out, so no validation switch
// may select it.
func TestUnit_ValidationLabelFilterNeverSelectsTheSiteScan(t *testing.T) {
	values := []string{"", "true", "false"}
	for _, site := range values {
		for _, tenant := range values {
			env := map[string]string{}
			if site != "" {
				env[siteValidateEnv] = site
			}
			if tenant != "" {
				env[tenantValidateEnv] = tenant
			}
			filter, err := validationLabelFilter(lookupIn(env))
			if err != nil || filter == "" {
				continue
			}
			matches, err := types.ParseLabelFilter(filter)
			require.NoError(t, err)
			require.False(t, matches([]string{cisSiteLabel}), "%s=%q %s=%q selects %s", siteValidateEnv, site, tenantValidateEnv, tenant, cisSiteLabel)
		}
	}
}

func TestUnit_CISSiteScanRequested(t *testing.T) {
	for name, tc := range map[string]struct {
		value string
		want  bool
	}{
		"unset":   {"", false},
		"true":    {"true", true},
		"padded":  {" true ", true},
		"false":   {"false", false},
		"garbage": {"yes", false},
	} {
		t.Run(name, func(t *testing.T) {
			getenv := func(key string) string {
				if key == cisSiteScanEnv {
					return tc.value
				}
				return ""
			}
			require.Equal(t, tc.want, cisSiteScanRequested(getenv))
		})
	}
}

func TestUnit_ValidationExclusions(t *testing.T) {
	for name, tc := range map[string]struct {
		filter, exclude, want string
	}{
		"nothing excluded leaves the filter alone": {"site-validate", "", "site-validate"},
		"one label":                {"site-validate", issuesCertificateLabel, "(site-validate) && !issues-certificate"},
		"spaced and repeated":      {"site-validate || tenant-validate", " issues-certificate , issues-certificate ", "(site-validate || tenant-validate) && !issues-certificate"},
		"no filter to add them to": {"", issuesCertificateLabel, "!issues-certificate"},
		"a caller's own filter":    {"site-software && !site-distro", issuesCertificateLabel, "(site-software && !site-distro) && !issues-certificate"},
	} {
		t.Run(name, func(t *testing.T) {
			exclude, err := validationExclusions(envMap(map[string]string{validateExcludeLabelsEnv: tc.exclude}))
			require.NoError(t, err)
			require.Equal(t, tc.want, withExclusions(tc.filter, exclude))
		})
	}
}

func TestUnit_ValidationExclusionsReject(t *testing.T) {
	for name, raw := range map[string]string{
		"the site switch's label":   siteValidateLabel,
		"the tenant switch's label": tenantValidateLabel,
		"a check every site needs":  "health",
		"a misspelling":             "issues-certficate",
		"one bad among good":        "issues-certificate,readiness",
		"an expression":             "issues-certificate || health",
		"a negation":                "!issues-certificate",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := validationExclusions(envMap(map[string]string{validateExcludeLabelsEnv: raw}))
			require.Error(t, err)
		})
	}
}
