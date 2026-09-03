// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"strings"
	"testing"
)

// TestRegistryMatchesProviderSources pins the registry to literals read from
// each provider's own repository. Asserting against literals rather than
// against the registry is the point: a test that compared the registry to
// itself would pass no matter what drifted.
//
// All three providers are under active development. When one is bumped,
// re-read its CRD names, its manager identity, and its credentials flags, and
// update this test with them.
func TestRegistryMatchesProviderSources(t *testing.T) {
	cases := []struct {
		typ                  Type
		clusterKind          string
		machineTemplateKind  string
		crdNames             []string
		appName              string
		controllerNamespace  string
		controllerDeployment string
		secretNameFlag       string
		defaultSecretName    string
		unconfiguredIsValid  bool
		supportsEnable       bool
	}{
		{
			typ:                  TypeNico,
			clusterKind:          "NicoCluster",
			machineTemplateKind:  "NicoMachineTemplate",
			crdNames:             []string{"nicoclusters.infrastructure.cluster.x-k8s.io", "nicomachinetemplates.infrastructure.cluster.x-k8s.io"},
			appName:              "cluster-api-provider-nico",
			controllerNamespace:  "capnico-system",
			controllerDeployment: "controller-manager",
			secretNameFlag:       "--provider-credentials-secret-name",
			defaultSecretName:    "nico-credentials",
			supportsEnable:       true,
		},
		{
			typ:                  TypeLaunchLayer,
			clusterKind:          "LaunchLayerCluster",
			machineTemplateKind:  "LaunchLayerMachineTemplate",
			crdNames:             []string{"launchlayerclusters.infrastructure.cluster.x-k8s.io", "launchlayermachinetemplates.infrastructure.cluster.x-k8s.io"},
			appName:              "cluster-api-provider-launchlayer",
			controllerNamespace:  "capll-system",
			controllerDeployment: "cluster-api-provider-launchlayer-controller-manager",
			secretNameFlag:       "--identity-secret",
			// CAPLL has no default Secret name: omitting --identity-secret
			// leaves its client unauthenticated on purpose.
			defaultSecretName:   "",
			unconfiguredIsValid: true,
			supportsEnable:      true,
		},
		{
			typ:                  TypeProvisioned,
			clusterKind:          "ProvisionedCluster",
			machineTemplateKind:  "ProvisionedMachineTemplate",
			crdNames:             []string{"provisionedclusters.infrastructure.cluster.x-k8s.io", "provisionedmachinetemplates.infrastructure.cluster.x-k8s.io"},
			appName:              "cluster-api-provider-provisioned",
			controllerNamespace:  "provisioned-system",
			controllerDeployment: "provisioned-controller-manager",
			supportsEnable:       false,
		},
	}

	for _, tc := range cases {
		t.Run(string(tc.typ), func(t *testing.T) {
			d, ok := Lookup(tc.typ)
			if !ok {
				t.Fatalf("Lookup(%q) found nothing", tc.typ)
			}
			if d.ClusterKind != tc.clusterKind {
				t.Errorf("ClusterKind = %q, want %q", d.ClusterKind, tc.clusterKind)
			}
			if d.MachineTemplateKind != tc.machineTemplateKind {
				t.Errorf("MachineTemplateKind = %q, want %q", d.MachineTemplateKind, tc.machineTemplateKind)
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
			if d.UnconfiguredIsValid != tc.unconfiguredIsValid {
				t.Errorf("UnconfiguredIsValid = %v, want %v", d.UnconfiguredIsValid, tc.unconfiguredIsValid)
			}
			if d.SupportsEnable != tc.supportsEnable {
				t.Errorf("SupportsEnable = %v, want %v", d.SupportsEnable, tc.supportsEnable)
			}
		})
	}
}

// TestRequestAPIVersionIsTheWireContract guards the apiVersion nkectl sends to
// CWE inside a capiInfraCluster payload. It is deliberately separate from the
// versions a provider serves: bumping this changes what CWE receives.
func TestRequestAPIVersionIsTheWireContract(t *testing.T) {
	for _, d := range All() {
		if d.RequestAPIVersion != "infrastructure.cluster.x-k8s.io/v1alpha1" {
			t.Errorf("%s RequestAPIVersion = %q, want infrastructure.cluster.x-k8s.io/v1alpha1",
				d.Type, d.RequestAPIVersion)
		}
	}
}

func TestLookupRejectsUnknownType(t *testing.T) {
	if _, ok := Lookup(Type("nope")); ok {
		t.Error("Lookup accepted a type that is not in the registry")
	}
}

func TestTypesListsEveryProvider(t *testing.T) {
	got := strings.Join(Types(), ",")
	want := "nico,launchlayer,provisioned"
	if got != want {
		t.Errorf("Types() = %q, want %q", got, want)
	}
}

// TestCWEProviderTypeIsRecorded checks that every entry states which of CWE's
// own provider_type values it is reached through. CWE distinguishes only "nico"
// from "capi", so without this the CLI's richer vocabulary could drift from the
// API it ultimately talks to.
func TestCWEProviderTypeIsRecorded(t *testing.T) {
	for _, d := range All() {
		switch d.CWEProviderType {
		case "nico", "capi":
		default:
			t.Errorf("%s CWEProviderType = %q, want nico or capi", d.Type, d.CWEProviderType)
		}
	}
}
