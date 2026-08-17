// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package dbreconcile_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile"
	dbsource "github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile/source"
)

func TestUnit_SetupController(t *testing.T) {
	mgr := &fakeManager{}
	reconciler := reconcile.TypedFunc[dbreconcile.Key](func(context.Context, dbreconcile.Key) (reconcile.Result, error) {
		return reconcile.Result{}, nil
	})

	err := dbreconcile.SetupController(mgr, "dbreconcile-test", reconciler, dbsource.NewSource[dbreconcile.Key](1))
	require.NoError(t, err)

	assert.Len(t, mgr.runnables, 1)
}

func TestUnit_SetupControllerRequiresSource(t *testing.T) {
	err := dbreconcile.SetupController(&fakeManager{}, "dbreconcile-test", reconcile.TypedFunc[dbreconcile.Key](
		func(context.Context, dbreconcile.Key) (reconcile.Result, error) {
			return reconcile.Result{}, nil
		},
	), nil)

	require.Error(t, err)
	assert.ErrorContains(t, err, "must specify Source")
}

func TestUnit_SetupControllerReturnsControllerError(t *testing.T) {
	err := dbreconcile.SetupController(&fakeManager{}, "", reconcile.TypedFunc[dbreconcile.Key](
		func(context.Context, dbreconcile.Key) (reconcile.Result, error) {
			return reconcile.Result{}, nil
		},
	), dbsource.NewSource[dbreconcile.Key](1))

	require.Error(t, err)
	assert.ErrorContains(t, err, "must specify Name")
}

type fakeManager struct {
	manager.Manager

	runnables []manager.Runnable
}

func (m *fakeManager) Add(runnable manager.Runnable) error {
	m.runnables = append(m.runnables, runnable)
	return nil
}

func (m *fakeManager) GetControllerOptions() config.Controller {
	return config.Controller{}
}
