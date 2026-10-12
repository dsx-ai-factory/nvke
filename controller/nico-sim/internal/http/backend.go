// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package http

import (
	"context"
	"errors"
	"fmt"
	nethttp "net/http"

	nicosdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"

	"github.com/dsx-ai-factory/nvke/controller/nico-sim/internal/apierr"
	"github.com/dsx-ai-factory/nvke/controller/nico-sim/internal/state"
)

// InstanceBackend handles instance operations for one backend.
type InstanceBackend interface {
	Create(ctx context.Context, site state.SiteRow, req nicosdk.InstanceCreateRequest) (state.Instance, error)
	ConvertToAPI(r *nethttp.Request, instance state.Instance) (nicosdk.Instance, error)
	Update(ctx context.Context, instance state.Instance, req nicosdk.InstanceUpdateRequest) (state.Instance, error)
	Delete(ctx context.Context, instance state.Instance, req *nicosdk.InstanceDeleteRequest) error
}

const instanceBackendLabel = "nico-sim.nvidia.com/backend"

func setInstanceBackendLabel(instance *nicosdk.Instance, backend state.SiteBackend) {
	current := instance.GetLabels()
	labels := make(map[string]string, len(current)+1)
	for key, value := range current {
		labels[key] = value
	}
	labels[instanceBackendLabel] = string(backend)
	instance.SetLabels(labels)
}

func createInstanceInput(site state.SiteRow, req nicosdk.InstanceCreateRequest) state.CreateInstanceInput {
	return state.CreateInstanceInput{
		Backend:                site.Backend,
		SiteID:                 site.ID,
		Name:                   req.GetName(),
		TenantID:               req.GetTenantId(),
		InstanceTypeID:         req.GetInstanceTypeId(),
		VPCID:                  req.GetVpcId(),
		MachineID:              req.GetMachineId(),
		IpxeScript:             req.GetIpxeScript(),
		UserData:               req.GetUserData(),
		Labels:                 req.GetLabels(),
		SSHKeyGroupIDs:         req.GetSshKeyGroupIds(),
		Interfaces:             interfaceRequests(req.GetInterfaces()),
		InfiniBandPartitionIDs: infiniBandPartitionIDs(req.GetInfinibandInterfaces()),
		PhoneHomeEnabled:       req.GetPhoneHomeEnabled(),
	}
}

func infiniBandPartitionIDs(requested []nicosdk.InfiniBandInterfaceCreateRequest) []string {
	partitionIDs := make([]string, 0, len(requested))
	for _, iface := range requested {
		if partitionID := iface.GetPartitionId(); partitionID != "" {
			partitionIDs = append(partitionIDs, partitionID)
		}
	}
	return partitionIDs
}

// errUnsupportedForBackend means the selected backend does not implement an endpoint.
var errUnsupportedForBackend = errors.New("unsupported for this backend")

// backendFor returns the backend configured for a site.
func (s *Server) backendFor(site state.SiteRow) (InstanceBackend, error) {
	switch site.Backend {
	case state.SiteBackendVM:
		return s.vmBackend, nil
	case state.SiteBackendContainer:
		return s.containerBackend, nil
	default:
		return nil, fmt.Errorf("%w: site %s has unsupported backend %q", apierr.ErrBadRequest, site.ID, site.Backend)
	}
}

func (s *Server) backendForInstance(instance state.Instance) (InstanceBackend, error) {
	switch instance.Backend {
	case state.SiteBackendVM:
		return s.vmBackend, nil
	case state.SiteBackendContainer:
		return s.containerBackend, nil
	default:
		return nil, fmt.Errorf("%w: instance %s has unsupported backend %q", apierr.ErrBadRequest, instance.ID, instance.Backend)
	}
}

// ownedInstance returns an instance after checking organization ownership.
func (s *Server) ownedInstance(r *nethttp.Request, instanceID string) (state.Instance, InstanceBackend, error) {
	instance, err := s.store.GetInstance(r.Context(), instanceID)
	if err != nil {
		return state.Instance{}, nil, err
	}
	if _, err := s.store.GetSiteForOrg(r.Context(), orgIDFromRequest(r), instance.SiteID); err != nil {
		return state.Instance{}, nil, err
	}
	backend, err := s.backendForInstance(instance)
	if err != nil {
		return state.Instance{}, nil, err
	}
	return instance, backend, nil
}

// assertVMBackend checks that an instance is VM-backed and returns its backend.
func (s *Server) assertVMBackend(r *nethttp.Request, instanceID string) (state.Instance, InstanceBackend, error) {
	instance, backend, err := s.ownedInstance(r, instanceID)
	if err != nil {
		return state.Instance{}, nil, err
	}
	if instance.VM == nil {
		return state.Instance{}, nil, fmt.Errorf("%w: %s instances have no such endpoint", errUnsupportedForBackend, instance.Backend)
	}
	return instance, backend, nil
}

// writeBackendError writes the HTTP response for a backend error.
func writeBackendError(w nethttp.ResponseWriter, err error) {
	switch {
	case errors.Is(err, apierr.ErrNotFound):
		writeForgeError(w, nethttp.StatusNotFound, "Could not find Instance with specified ID")
	case errors.Is(err, apierr.ErrBadRequest):
		writeForgeError(w, nethttp.StatusBadRequest, err.Error())
	case errors.Is(err, apierr.ErrOwnershipConflict):
		writeForgeError(w, nethttp.StatusConflict, "InstanceOwnershipConflict: "+err.Error())
	case errors.Is(err, apierr.ErrConflict):
		writeForgeError(w, nethttp.StatusConflict, err.Error())
	case errors.Is(err, apierr.ErrUnavailable):
		writeForgeError(w, nethttp.StatusServiceUnavailable, "BackendUnavailable: "+err.Error())
	case errors.Is(err, errUnsupportedForBackend):
		writeForgeError(w, nethttp.StatusNotImplemented, "UnsupportedBackendOperation: "+err.Error())
	default:
		writeForgeError(w, nethttp.StatusInternalServerError, err.Error())
	}
}

// interfaceRequests converts SDK interface requests into database inputs.
func interfaceRequests(requested []nicosdk.InterfaceCreateRequest) []state.InterfaceRequest {
	interfaces := make([]state.InterfaceRequest, 0, len(requested))
	for _, iface := range requested {
		request := state.InterfaceRequest{}
		if subnetID := iface.GetSubnetId(); subnetID != "" {
			request.SubnetID = &subnetID
		}
		if vpcPrefixID := iface.GetVpcPrefixId(); vpcPrefixID != "" {
			request.VPCPrefixID = &vpcPrefixID
		}
		interfaces = append(interfaces, request)
	}
	return interfaces
}
