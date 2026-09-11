// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package http

import (
	"context"
	"errors"
	"fmt"
	nethttp "net/http"

	nicosdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"

	"github.com/NVIDIA/nke/controller/nico-sim/internal/apierr"
	"github.com/NVIDIA/nke/controller/nico-sim/internal/state"
)

// containerBackend keeps database records in sync with Docker containers.
type containerBackend struct {
	store      *state.Store
	containers ContainerService
}

func (b containerBackend) Create(ctx context.Context, site state.SiteRow, req nicosdk.InstanceCreateRequest) (state.Instance, error) {
	input := createInstanceInput(site, req)
	input.Container = &state.ContainerState{}
	record, created, err := b.store.CreateInstance(ctx, input)
	if err != nil {
		return state.Instance{}, err
	}
	if !created {
		return record, nil
	}
	_, err = b.containers.Create(ctx, record.ID, req)
	if err != nil {
		// The row exists only to name a container. Drop it so a failed create
		// leaves nothing behind and the name is free to retry.
		if deleteErr := b.store.DeleteInstance(context.WithoutCancel(ctx), record.ID); deleteErr != nil {
			return state.Instance{}, errors.Join(err, fmt.Errorf("rolling back instance row: %w", deleteErr))
		}
		return state.Instance{}, err
	}
	return record, nil
}

func (b containerBackend) ConvertToAPI(r *nethttp.Request, record state.Instance) (nicosdk.Instance, error) {
	if record.Container == nil {
		return nicosdk.Instance{}, fmt.Errorf("%w: instance %s has no container state", apierr.ErrOwnershipConflict, record.ID)
	}
	instance, err := b.containers.Get(r.Context(), record.ID)
	if err != nil {
		return nicosdk.Instance{}, err
	}
	return withInstanceRecord(*instance, record)
}

func (b containerBackend) Update(ctx context.Context, record state.Instance, req nicosdk.InstanceUpdateRequest) (state.Instance, error) {
	_, err := b.containers.Update(ctx, record.ID, req)
	if err != nil {
		return state.Instance{}, err
	}
	row, err := b.store.UpdateInstance(ctx, record.ID, state.UpdateInstanceInput{Labels: req.GetLabels()})
	if err != nil {
		return state.Instance{}, err
	}
	return row, nil
}

func (b containerBackend) Delete(ctx context.Context, record state.Instance, req *nicosdk.InstanceDeleteRequest) error {
	if err := b.containers.Delete(ctx, record.ID, req); err != nil {
		return err
	}
	return b.store.DeleteInstance(ctx, record.ID)
}

// withInstanceRecord uses the database for fields shared by both backends.
func withInstanceRecord(instance nicosdk.Instance, row state.Instance) (nicosdk.Instance, error) {
	labels, err := row.Labels()
	if err != nil {
		return nicosdk.Instance{}, err
	}
	if len(labels) > 0 {
		instance.SetLabels(labels)
	}
	setInstanceBackendLabel(&instance, row.Backend)
	if row.IpxeScript != "" {
		instance.SetIpxeScript(row.IpxeScript)
	}
	if row.UserData != "" {
		instance.SetUserData(row.UserData)
	}
	instance.SetPhoneHomeEnabled(row.PhoneHomeEnabled)
	return instance, nil
}

var errContainersDisabled = fmt.Errorf("%w: container backend is disabled", apierr.ErrUnavailable)

// DisabledContainers returns unavailable for every operation.
type DisabledContainers struct{}

func (DisabledContainers) Create(context.Context, string, nicosdk.InstanceCreateRequest) (*nicosdk.Instance, error) {
	return nil, errContainersDisabled
}

func (DisabledContainers) Get(context.Context, string) (*nicosdk.Instance, error) {
	return nil, errContainersDisabled
}

func (DisabledContainers) Update(context.Context, string, nicosdk.InstanceUpdateRequest) (*nicosdk.Instance, error) {
	return nil, errContainersDisabled
}

func (DisabledContainers) Delete(context.Context, string, *nicosdk.InstanceDeleteRequest) error {
	return errContainersDisabled
}
