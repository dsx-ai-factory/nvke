// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package credentials

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// nicoIdentity builds a NicoIdentity as the API server returns it, with the
// given status. A nil status leaves the field out, as before the first check.
func nicoIdentity(status map[string]any) *unstructured.Unstructured {
	obj := map[string]any{
		"apiVersion": "infrastructure.cluster.x-k8s.io/v1alpha1",
		"kind":       "NicoIdentity",
		"metadata": map[string]any{
			"name":              "nico-default",
			"namespace":         "capnico-system",
			"generation":        int64(2),
			"creationTimestamp": "2026-10-06T14:00:00Z",
			"labels":            map[string]any{"cluster.x-k8s.io/watch-filter": "site-a"},
		},
		"spec": map[string]any{
			"credentialsRef": map[string]any{"name": "nico-credentials"},
		},
	}
	if status != nil {
		obj["status"] = status
	}
	return &unstructured.Unstructured{Object: obj}
}

func TestDecodeReadsTheFieldsClassificationUses(t *testing.T) {
	id := decode(nicoIdentity(map[string]any{
		"conditions": []any{
			map[string]any{"type": "Paused", "status": "False"},
			map[string]any{
				"type":               "Ready",
				"status":             "False",
				"reason":             "AuthenticationFailed",
				"message":            "Current-tenant lookup rejected the access token.",
				"observedGeneration": int64(2),
				"lastTransitionTime": "2026-10-06T15:00:00Z",
			},
		},
		"lastCheckedTime": "2026-10-06T15:04:30Z",
	}))

	if id.Namespace != "capnico-system" || id.Name != "nico-default" || id.Generation != 2 {
		t.Errorf("identity = %s/%s generation %d, want capnico-system/nico-default generation 2", id.Namespace, id.Name, id.Generation)
	}
	if want := time.Date(2026, 10, 6, 14, 0, 0, 0, time.UTC); !id.Created.Equal(want) {
		t.Errorf("Created = %v, want %v", id.Created, want)
	}
	if id.SecretName != "nico-credentials" {
		t.Errorf("SecretName = %q, want nico-credentials", id.SecretName)
	}
	if id.Labels["cluster.x-k8s.io/watch-filter"] != "site-a" {
		t.Errorf("Labels = %v, want the watch-filter label", id.Labels)
	}
	if id.Ready == nil {
		t.Fatal("Ready = nil, want the Ready condition found among others")
	}
	if id.Ready.Status != "False" || id.Ready.Reason != "AuthenticationFailed" || id.Ready.ObservedGeneration != 2 {
		t.Errorf("Ready = %+v, want False/AuthenticationFailed at generation 2", id.Ready)
	}
	if id.Ready.Message != "Current-tenant lookup rejected the access token." {
		t.Errorf("Ready.Message = %q", id.Ready.Message)
	}
	if want := time.Date(2026, 10, 6, 15, 0, 0, 0, time.UTC); id.Ready.LastTransitionTime == nil || !id.Ready.LastTransitionTime.Time.Equal(want) {
		t.Errorf("Ready.LastTransitionTime = %v, want 2026-10-06T15:00:00Z", id.Ready.LastTransitionTime)
	}
	if want := time.Date(2026, 10, 6, 15, 4, 30, 0, time.UTC); id.LastChecked == nil || !id.LastChecked.Equal(want) {
		t.Errorf("LastChecked = %v, want %v", id.LastChecked, want)
	}
	if id.Deleting || id.LastCheckedInvalid {
		t.Errorf("Deleting = %v, LastCheckedInvalid = %v, want both false", id.Deleting, id.LastCheckedInvalid)
	}
}
