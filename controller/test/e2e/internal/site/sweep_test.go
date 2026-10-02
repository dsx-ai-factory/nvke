// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package site

import (
	"context"
	"errors"
	"strings"
	"testing"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/dsx-ai-factory/nke/controller/api"
	"github.com/dsx-ai-factory/nke/controller/test/e2e/internal/nke"
)

var _ Tenants = (*nke.Client)(nil)

type fakeTenants struct {
	clusters []api.Cluster
	failFor  openapi_types.UUID
	calls    []string
}

func (f *fakeTenants) ListClusters(context.Context) ([]api.Cluster, error) { return f.clusters, nil }

func (f *fakeTenants) DeleteCluster(_ context.Context, id openapi_types.UUID) error {
	f.calls = append(f.calls, "delete "+id.String())
	if id == f.failFor {
		return errors.New("unexpected status 500")
	}
	return nil
}

func (f *fakeTenants) WaitForClusterDeleted(_ context.Context, id openapi_types.UUID) error {
	f.calls = append(f.calls, "wait "+id.String())
	return nil
}

func TestSweepTenants(t *testing.T) {
	a, b, c := openapi_types.UUID{1}, openapi_types.UUID{2}, openapi_types.UUID{3}
	f := &fakeTenants{
		clusters: []api.Cluster{{ID: a, Name: "a"}, {ID: b, Name: "b"}, {ID: c, Name: "c"}},
		failFor:  b,
	}
	err := SweepTenants(context.Background(), f)
	if err == nil || !strings.Contains(err.Error(), "delete tenant b") {
		t.Fatalf("err = %v", err)
	}
	want := []string{"delete " + a.String(), "delete " + b.String(), "delete " + c.String(), "wait " + a.String(), "wait " + c.String()}
	if strings.Join(f.calls, ",") != strings.Join(want, ",") {
		t.Errorf("calls = %v, want %v", f.calls, want)
	}
}

func TestSweepTenantsNone(t *testing.T) {
	if err := SweepTenants(context.Background(), &fakeTenants{}); err != nil {
		t.Fatal(err)
	}
}
