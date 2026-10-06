// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
)

func testMachineInit() MachineInit {
	return MachineInit{UserDataTemplate: "#cloud-config\nhostname: {{ .Hostname }}\n"}
}

func TestTypedResourcesRoundTrip(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	codecs := serializer.NewCodecFactory(scheme)
	zero := int32(0)

	for _, tt := range []struct {
		resource string
		kind     string
		object   runtime.Object
		// verify checks what is specific to this kind, after the round-trip
		// assertions every kind shares.
		verify func(t *testing.T, decoded runtime.Object)
	}{
		{
			resource: "nkeclusters", kind: "NKECluster",
			object: &NKECluster{
				Name: "demo", Namespace: "org-demo",
				Spec: NKEClusterSpec{
					ClusterNetwork: ClusterNetwork{
						Pods:          NetworkRanges{CIDRBlocks: []NetworkCIDR{"10.244.0.0/16"}},
						Services:      NetworkRanges{CIDRBlocks: []NetworkCIDR{"10.96.0.0/12"}},
						ServiceDomain: "cluster.local",
					},
					Release:            ReleaseSelection{Channel: ReleaseChannelReference{Name: "stable"}},
					ControlPlaneClass:  "standard",
					AcceleratedRuntime: &AcceleratedRuntimeSelection{Name: "gb200-ubuntu", Version: "1.2.3"},
					Network: &Network{
						VPCID: "147ca92b-a90f-4094-92cd-7a0b3f10c30d",
					},
					NodePools: []NodePool{{
						Name: "workers", Replicas: &zero, InstanceType: "gpu-small", NodeProfile: "ubuntu:2026.09.1",
						VPCSubnetID: "b91c0605-17aa-4ade-aa2c-6a5eb1601dcd",
					}},
				},
			},
			verify: func(t *testing.T, decoded runtime.Object) {
				if replicas := decoded.(*NKECluster).Spec.NodePools[0].Replicas; replicas == nil || *replicas != 0 {
					t.Fatal("explicit zero replicas lost during round-trip")
				}
			},
		},
		{
			resource: "machineimages", kind: "MachineImage",
			object: &MachineImage{
				Name: "nke-nico-vms-24.04-amd64-fde",
				Spec: MachineImageSpec{
					OperatingSystem: OperatingSystem{Family: "ubuntu", Version: "24.04"},
					Architecture:    "amd64",
					Variant:         "fde",
					BuildDate:       "2026-03-23",
					NICo:            NICoBoot{IPXEScript: "#!ipxe\nboot\n"},
				},
			},
		},
		{
			resource: "clusterreleases", kind: "ClusterRelease",
			object: &ClusterRelease{
				Name: "nke-nico-vms-24.04-amd64-fde",
				Spec: ClusterReleaseSpec{
					MachineImageRef: MachineImageReference{Name: "nke-nico-vms-24.04-amd64-fde"},
					MachineInit:     testMachineInit(),
				},
			},
		},
		{
			resource: "nodepoolreleases", kind: "NodePoolRelease",
			object: &NodePoolRelease{
				Name: "nke-nico-vms-24.04-amd64-fde",
				Spec: NodePoolReleaseSpec{
					MachineImageRef: MachineImageReference{Name: "nke-nico-vms-24.04-amd64-fde"},
					MachineInit:     testMachineInit(),
				},
			},
		},
	} {
		t.Run(tt.kind, func(t *testing.T) {
			encoded, err := runtime.Encode(codecs.LegacyCodec(GroupVersion), tt.object)
			if err != nil {
				t.Fatal(err)
			}
			decoded, gvk, err := codecs.UniversalDeserializer().Decode(encoded, nil, nil)
			if err != nil || *gvk != GroupVersion.WithKind(tt.kind) {
				t.Fatalf("decode: kind=%v error=%v", gvk, err)
			}
			value := decodeJSON[map[string]any](t, encoded)
			if _, present := value["status"]; present {
				t.Fatal("unobserved status must be omitted")
			}
			// What the Go types emit must be admissible by the generated CRD.
			loadResourceSchema(t, tt.resource).admit(t, value, nil)
			if tt.verify != nil {
				tt.verify(t, decoded)
			}
		})
	}

	for _, kind := range []string{"NKEClusterList", "MachineImageList", "ClusterReleaseList", "NodePoolReleaseList"} {
		if _, err := scheme.New(GroupVersion.WithKind(kind)); err != nil {
			t.Fatal(err)
		}
	}
	for _, kind := range []string{"NodePool", "NodePoolList"} {
		if _, err := scheme.New(GroupVersion.WithKind(kind)); !runtime.IsNotRegisteredError(err) {
			t.Fatalf("removed kind %s is still registered: %v", kind, err)
		}
	}
}

// TestAuthenticationRoundTripPreservesImmutability guards the create-only rule
// against its most likely false positive: a controller that decodes a cluster,
// edits something unrelated and writes it back must not look like an
// authentication change just because the value passed through the Go types.
func TestAuthenticationRoundTripPreservesImmutability(t *testing.T) {
	schema := loadResourceSchema(t, "nkeclusters")

	for _, tt := range []struct {
		name string
		// auth is spec.authentication as JSON; empty leaves the field absent.
		auth string
	}{
		{name: "absent"},
		{name: "no issuers", auth: `{}`},
		{
			name: "one issuer",
			auth: `{"jwt":[{"issuer":{"url":"https://issuer.example.com","audiences":["nke"]},
			                 "claimMappings":{"username":{"claim":"sub"}}}]}`,
		},
		{
			name: "empty prefix and required value",
			auth: `{"jwt":[{"issuer":{"url":"https://issuer.example.com","audiences":["nke"]},
			                 "claimMappings":{"username":{"claim":"sub","prefix":""}},
			                 "claimValidationRules":[{"claim":"organization","requiredValue":""}]}]}`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			old := baseResource(t, "nkeclusters")
			if tt.auth != "" {
				setField(old, []string{"spec", "authentication"}, decodeJSON[map[string]any](t, []byte(tt.auth)))
			}
			schema.admit(t, old, nil)

			cluster := decodeJSON[NKECluster](t, encodeJSON(t, old))
			cluster.Spec.APIServerAccess.PublicAccessEnabled = true
			updated := decodeJSON[map[string]any](t, encodeJSON(t, cluster))
			if errs := schema.validate(updated, old); len(errs) != 0 {
				t.Fatalf("unrelated typed update changed authentication: %v", errs)
			}

			if cluster.Spec.Authentication == nil || len(cluster.Spec.Authentication.JWT) == 0 {
				return
			}
			// A real edit to the same fields must still be refused, so the
			// round-trip above passes because nothing changed, not because the
			// rule is toothless.
			for _, mutation := range []struct {
				name  string
				apply func(jwt map[string]any)
			}{
				{"username prefix", func(jwt map[string]any) {
					setField(jwt, []string{"claimMappings", "username", "prefix"}, "changed:")
				}},
				{"claim validation rules", func(jwt map[string]any) {
					jwt["claimValidationRules"] = []any{map[string]any{"claim": "organization", "requiredValue": "changed"}}
				}},
			} {
				t.Run(mutation.name, func(t *testing.T) {
					changed := runtime.DeepCopyJSON(updated)
					mutation.apply(firstAuthenticator(t, changed))
					schema.check(t, true, changed, old)
				})
			}
		})
	}
}

// firstAuthenticator returns spec.authentication.jwt[0] for in-place mutation.
func firstAuthenticator(t *testing.T, obj map[string]any) map[string]any {
	t.Helper()
	spec, _ := obj["spec"].(map[string]any)
	authentication, _ := spec["authentication"].(map[string]any)
	jwt, _ := authentication["jwt"].([]any)
	if len(jwt) == 0 {
		t.Fatal("resource carries no JWT authenticators")
	}
	first, ok := jwt[0].(map[string]any)
	if !ok {
		t.Fatalf("jwt[0] is %T, want an object", jwt[0])
	}
	return first
}

// Deprecating a release goes through the Go types, so a decode/edit/encode
// cycle must not itself look like a change to the immutable content. Without
// this, the spec-level rule could pass its own unit tests and still reject
// every write a controller or nkectl makes.
func TestLifecycleEditRoundTripsThroughGoTypes(t *testing.T) {
	for _, resource := range releaseResources {
		t.Run(resource, func(t *testing.T) {
			schema := loadResourceSchema(t, resource)
			published := baseResource(t, resource)
			schema.admit(t, published, nil)

			var updated map[string]any
			switch resource {
			case "clusterreleases":
				release := decodeJSON[ClusterRelease](t, encodeJSON(t, published))
				release.Spec.Lifecycle.Deprecated = true
				updated = decodeJSON[map[string]any](t, encodeJSON(t, release))
			default:
				release := decodeJSON[NodePoolRelease](t, encodeJSON(t, published))
				release.Spec.Lifecycle.Deprecated = true
				updated = decodeJSON[map[string]any](t, encodeJSON(t, release))
			}
			schema.admit(t, updated, published)

			// The same round trip that permits the deprecation must still
			// refuse a content edit, so the check above is not vacuous.
			specOf(t, updated)["machineImageRef"] = map[string]any{"name": "other-image"}
			schema.check(t, true, updated, published)
		})
	}
}
