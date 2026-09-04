// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"strings"
	"testing"
)

// TestSupportedMatchesProviderSources pins the supported set to literals read
// from each provider's own repository. Asserting against literals is the point:
// a test that compared the set to itself would pass no matter what drifted.
//
// Both providers are under active development. When one is bumped,
// re-read its CRD names, its manager identity, and its credentials flags, and
// update this test with them.
func TestSupportedMatchesProviderSources(t *testing.T) {
	cases := []struct {
		typ                  Type
		crdNames             []string
		appName              string
		controllerNamespace  string
		controllerDeployment string
		secretNameFlag       string
		defaultSecretName    string
		endpointFlag         string
		unconfiguredIsValid  bool
	}{
		{
			typ:                  TypeNico,
			crdNames:             []string{"nicoclusters.infrastructure.cluster.x-k8s.io", "nicomachinetemplates.infrastructure.cluster.x-k8s.io"},
			appName:              "cluster-api-provider-nico",
			controllerNamespace:  "capnico-system",
			controllerDeployment: "controller-manager",
			secretNameFlag:       "--provider-credentials-secret-name",
			defaultSecretName:    "nico-credentials",
			// NICo carries its endpoint in the Secret, not in a manager flag.
			endpointFlag: "",
		},
		{
			typ:                  TypeLaunchLayer,
			crdNames:             []string{"launchlayerclusters.infrastructure.cluster.x-k8s.io", "launchlayermachinetemplates.infrastructure.cluster.x-k8s.io"},
			appName:              "cluster-api-provider-launchlayer",
			controllerNamespace:  "capll-system",
			controllerDeployment: "cluster-api-provider-launchlayer-controller-manager",
			secretNameFlag:       "--identity-secret",
			// CAPLL has no default Secret name: omitting --identity-secret
			// leaves its client unauthenticated on purpose.
			defaultSecretName:   "",
			endpointFlag:        "--base-url",
			unconfiguredIsValid: true,
		},
	}

	for _, tc := range cases {
		t.Run(string(tc.typ), func(t *testing.T) {
			var d Provider
			var found bool
			for _, p := range Supported() {
				if p.Type == tc.typ {
					d, found = p, true
				}
			}
			if !found {
				t.Fatalf("%s is not in the supported set", tc.typ)
			}
			if strings.Join(d.CRDNames, ",") != strings.Join(tc.crdNames, ",") {
				t.Errorf("CRDNames = %v, want %v", d.CRDNames, tc.crdNames)
			}
			if d.AppName != tc.appName {
				t.Errorf("AppName = %q, want %q", d.AppName, tc.appName)
			}
			if d.ControllerNamespace != tc.controllerNamespace {
				t.Errorf("ControllerNamespace = %q, want %q", d.ControllerNamespace, tc.controllerNamespace)
			}
			if d.ControllerDeployment != tc.controllerDeployment {
				t.Errorf("ControllerDeployment = %q, want %q", d.ControllerDeployment, tc.controllerDeployment)
			}
			if d.SecretNameFlag != tc.secretNameFlag {
				t.Errorf("SecretNameFlag = %q, want %q", d.SecretNameFlag, tc.secretNameFlag)
			}
			if d.DefaultSecretName != tc.defaultSecretName {
				t.Errorf("DefaultSecretName = %q, want %q", d.DefaultSecretName, tc.defaultSecretName)
			}
			if d.EndpointFlag != tc.endpointFlag {
				t.Errorf("EndpointFlag = %q, want %q", d.EndpointFlag, tc.endpointFlag)
			}
			if d.UnconfiguredIsValid != tc.unconfiguredIsValid {
				t.Errorf("UnconfiguredIsValid = %v, want %v", d.UnconfiguredIsValid, tc.unconfiguredIsValid)
			}
		})
	}
}

// TestSupportedSetIsTheTwoProvidersNkectlKnows guards the set itself. Adding a
// provider is a deliberate change to what this build claims to support, so it
// should break this test and be updated with it.
func TestSupportedSetIsTheTwoProvidersNkectlKnows(t *testing.T) {
	var names []string
	for _, p := range Supported() {
		names = append(names, string(p.Type))
	}
	if got, want := strings.Join(names, ","), "nico,launchlayer"; got != want {
		t.Errorf("Supported() = %q, want %q", got, want)
	}
}
