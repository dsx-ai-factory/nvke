// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clients

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/NVIDIA/nke/cli/internal/config"
	api "github.com/NVIDIA/nke/controller/api"
)

func TestUpdateClusterSendsPatchAndReturnsTheUpdatedCluster(t *testing.T) {
	id := uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/api/clusters/"+id.String() {
			t.Errorf("request = %s %s, want PATCH /api/clusters/%s", r.Method, r.URL.Path, id)
		}
		var got api.UpdateClusterJSONRequestBody
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decoding request body: %v", err)
		}
		if got.ControlPlaneKubernetesVersion == nil || *got.ControlPlaneKubernetesVersion != "1.31.0" {
			t.Errorf("controlPlaneKubernetesVersion = %v, want 1.31.0", got.ControlPlaneKubernetesVersion)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(api.Cluster{ID: id, Name: "updated-cluster"})
	}))
	defer srv.Close()

	c, err := New(config.SiteConfig{URL: srv.URL})
	if err != nil {
		t.Fatalf("building client: %v", err)
	}

	version := "1.31.0"
	got, err := c.UpdateCluster(context.Background(), id, api.UpdateClusterJSONRequestBody{
		ControlPlaneKubernetesVersion: &version,
	})
	if err != nil {
		t.Fatalf("UpdateCluster: %v", err)
	}
	if got.Name != "updated-cluster" {
		t.Errorf("returned cluster name = %q, want updated-cluster", got.Name)
	}
}

func TestUpdateClusterSurfacesAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"controlPlaneKubernetesVersion must be greater than the current version"}`))
	}))
	defer srv.Close()

	c, err := New(config.SiteConfig{URL: srv.URL})
	if err != nil {
		t.Fatalf("building client: %v", err)
	}

	_, err = c.UpdateCluster(context.Background(), uuid.New(), api.UpdateClusterJSONRequestBody{})
	if err == nil {
		t.Fatal("UpdateCluster against a 400 server returned no error")
	}
	if got := err.Error(); got == "" {
		t.Fatal("error message is empty")
	}
}
