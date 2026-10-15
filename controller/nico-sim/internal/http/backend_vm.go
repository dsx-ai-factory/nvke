// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package http

import (
	"context"
	"fmt"
	"net"
	nethttp "net/http"

	nicosdk "github.com/NVIDIA/infra-controller/rest-api/sdk/standard"

	"github.com/dsx-ai-factory/nvke/controller/nico-sim/internal/apierr"
	"github.com/dsx-ai-factory/nvke/controller/nico-sim/internal/state"
)

// vmBackend adapts the VM store and reconciliation engine to InstanceBackend.
// The VM-shaped work lives here so handlers never see it.
type vmBackend struct {
	store          *state.Store
	consoleSSHPort int
}

func (b vmBackend) Create(ctx context.Context, site state.SiteRow, req nicosdk.InstanceCreateRequest) (state.Instance, error) {
	if unsupported := unsupportedVMCreateField(req); unsupported != "" {
		return state.Instance{}, fmt.Errorf("%w: %s is not supported by the VM backend", apierr.ErrBadRequest, unsupported)
	}
	input := createInstanceInput(site, req)
	input.VM = &state.VMState{DesiredState: state.DesiredStatePresent, ObservedState: state.ObservedStateNew}
	row, _, err := b.store.CreateInstance(ctx, input)
	if err != nil {
		return state.Instance{}, err
	}
	return row, nil
}

func (b vmBackend) ConvertToAPI(r *nethttp.Request, instance state.Instance) (nicosdk.Instance, error) {
	if instance.VM == nil {
		return nicosdk.Instance{}, fmt.Errorf("%w: instance %s has no VM state", apierr.ErrOwnershipConflict, instance.ID)
	}
	presented := b.present(instance)
	if b.consoleSSHPort <= 0 {
		return presented, nil
	}
	host := r.Host
	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		host = parsedHost
	}
	if host == "" {
		host = "localhost"
	}
	presented.SetSerialConsoleUrl(fmt.Sprintf("ssh://%s@%s:%d", presented.GetId(), host, b.consoleSSHPort))
	return presented, nil
}

func (b vmBackend) Update(ctx context.Context, instance state.Instance, req nicosdk.InstanceUpdateRequest) (state.Instance, error) {
	var ipxeScript, userData *string
	if req.HasIpxeScript() {
		value := req.GetIpxeScript()
		ipxeScript = &value
	}
	if req.HasUserData() {
		value := req.GetUserData()
		userData = &value
	}
	row, err := b.store.UpdateInstance(ctx, instance.ID, state.UpdateInstanceInput{
		Labels:        req.GetLabels(),
		TriggerReboot: req.GetTriggerReboot(),
		IpxeScript:    ipxeScript,
		UserData:      userData,
	})
	if err != nil {
		return state.Instance{}, err
	}
	return row, nil
}

func (b vmBackend) Delete(ctx context.Context, instance state.Instance, _ *nicosdk.InstanceDeleteRequest) error {
	found, err := b.store.MarkVMInstanceForDelete(ctx, instance.ID)
	if err != nil {
		return err
	}
	if !found {
		return apierr.ErrNotFound
	}
	return nil
}

// present renders a stored VM, including the serial console URL that only VMs
// have. The reconciliation engine performs the deletion itself.
func (b vmBackend) present(row state.Instance) nicosdk.Instance {
	return toSDKInstance(row)
}

// unsupportedVMCreateField names the first request field the VM lifecycle
// cannot honour, or "" when the request is entirely supported.
func unsupportedVMCreateField(req nicosdk.InstanceCreateRequest) string {
	if req.HasMachineId() {
		return "machineId"
	}
	if len(req.GetSecondaryVpcIds()) > 0 {
		return "secondaryVpcIds"
	}
	if len(req.GetNvLinkInterfaces()) > 0 {
		return "nvLinkInterfaces"
	}
	if len(req.GetDpuExtensionServiceDeployments()) > 0 {
		return "dpuExtensionServiceDeployments"
	}
	if req.HasAllowUnhealthyMachine() {
		return "allowUnhealthyMachine"
	}
	for _, iface := range req.GetInterfaces() {
		if iface.HasIpAddress() || iface.HasDevice() || iface.HasDeviceInstance() || iface.HasVirtualFunctionId() {
			return "detailed interface placement"
		}
	}
	return ""
}
