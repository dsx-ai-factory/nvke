// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
)

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
				ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "org-demo"},
				Spec: NKEClusterSpec{
					ClusterNetwork: ClusterNetwork{
						Pods:          NetworkRanges{CIDRBlocks: []NetworkCIDR{"10.244.0.0/16"}},
						Services:      NetworkRanges{CIDRBlocks: []NetworkCIDR{"10.96.0.0/12"}},
						ServiceDomain: "cluster.local",
					},
					Release:           ClusterReleaseSelection{Channel: ReleaseChannelReference{Name: "stable"}},
					ControlPlaneClass: "standard",
				},
			},
		},
		{
			resource: "nodepools", kind: "NodePool",
			object: &NodePool{
				ObjectMeta: metav1.ObjectMeta{Name: "workers", Namespace: "org-demo"},
				Spec: NodePoolSpec{
					ClusterName: "demo", Replicas: &zero, InstanceType: "gpu-small",
					Release: ReleaseReference{Name: "ubuntu", Version: "2026.09.1"},
				},
			},
			verify: func(t *testing.T, decoded runtime.Object) {
				if replicas := decoded.(*NodePool).Spec.Replicas; replicas == nil || *replicas != 0 {
					t.Fatal("explicit zero replicas lost during round-trip")
				}
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

	for _, kind := range []string{"NKEClusterList", "NodePoolList"} {
		if _, err := scheme.New(GroupVersion.WithKind(kind)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNodePoolDeepCopyIsolatesMutableFields(t *testing.T) {
	replicas := int32(2)
	original := &NodePool{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"team": "research"}},
		Spec: NodePoolSpec{
			Replicas: &replicas,
			Template: &NodeTemplate{Metadata: &NodeTemplateMetadata{Labels: map[string]LabelValue{"workload": "training"}}},
		},
		Status: NodePoolStatus{ClusterRef: &ClusterReference{Name: "demo", UID: "original"}},
	}

	clone := original.DeepCopy()
	clone.Labels["team"] = "other"
	*clone.Spec.Replicas = 0
	clone.Spec.Template.Metadata.Labels["workload"] = "inference"
	clone.Status.ClusterRef.UID = "other"

	if original.Labels["team"] != "research" || *original.Spec.Replicas != 2 ||
		original.Spec.Template.Metadata.Labels["workload"] != "training" || original.Status.ClusterRef.UID != "original" {
		t.Fatal("deepcopy mutation changed the cached resource")
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
