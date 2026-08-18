// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package http

import (
	"encoding/json"
	"errors"
	nethttp "net/http"

	forgesdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"

	"github.com/NVIDIA/nke/controller/forge-vms/internal/state"
)

func (s *Server) handleGetInstance(w nethttp.ResponseWriter, r *nethttp.Request) {
	id := r.PathValue("instanceId")

	row, err := s.store.GetInstanceByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			writeForgeError(w, nethttp.StatusNotFound, "Could not find Instance with specified ID")
			return
		}
		writeForgeError(w, nethttp.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, nethttp.StatusOK, s.instanceWithConsoleURL(toSDKInstance(row), r))
}

func (s *Server) handleListInstances(w nethttp.ResponseWriter, r *nethttp.Request) {
	page := parsePage(r)
	filter := state.ListInstancesFilter{
		SiteID:   r.URL.Query().Get("siteId"),
		TenantID: r.URL.Query().Get("tenantId"),
	}

	rows, err := s.store.ListInstances(r.Context(), filter, page)
	if err != nil {
		writeForgeError(w, nethttp.StatusInternalServerError, err.Error())
		return
	}

	resp := make([]forgesdk.Instance, 0, len(rows))
	for _, row := range rows {
		resp = append(resp, s.instanceWithConsoleURL(toSDKInstance(row), r))
	}
	writeJSON(w, nethttp.StatusOK, resp)
}

func (s *Server) handleCreateInstance(w nethttp.ResponseWriter, r *nethttp.Request) {
	r.Body = nethttp.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	var req forgesdk.InstanceCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeForgeError(w, nethttp.StatusBadRequest, "invalid request body")
		return
	}

	interfaces := make([]state.InterfaceRequest, 0, len(req.GetInterfaces()))
	for _, iface := range req.GetInterfaces() {
		var subnetID *string
		var vpcPrefixID *string
		if iface.HasSubnetId() {
			v := iface.GetSubnetId()
			subnetID = &v
		}
		if iface.HasVpcPrefixId() {
			v := iface.GetVpcPrefixId()
			vpcPrefixID = &v
		}
		interfaces = append(interfaces, state.InterfaceRequest{
			SubnetID:    subnetID,
			VPCPrefixID: vpcPrefixID,
		})
	}

	in := state.CreateInstanceInput{
		Name:             req.GetName(),
		TenantID:         req.GetTenantId(),
		InstanceTypeID:   req.GetInstanceTypeId(),
		VPCID:            req.GetVpcId(),
		IpxeScript:       req.GetIpxeScript(),
		UserData:         req.GetUserData(),
		Labels:           req.GetLabels(),
		SSHKeyGroupIDs:   req.GetSshKeyGroupIds(),
		Interfaces:       interfaces,
		PhoneHomeEnabled: req.GetPhoneHomeEnabled(),
	}

	row, _, err := s.store.CreateInstance(r.Context(), in)
	if err != nil {
		switch {
		case errors.Is(err, state.ErrConflict):
			writeForgeError(w, nethttp.StatusConflict, "An Instance with specified name already exists for Tenant")
		case errors.Is(err, state.ErrSubnetExhausted):
			writeForgeError(w, nethttp.StatusInternalServerError, err.Error())
		case errors.Is(err, state.ErrVpcPfxExhausted):
			writeForgeError(w, nethttp.StatusInternalServerError, err.Error())
		case errors.Is(err, state.ErrBadRequest):
			writeForgeError(w, nethttp.StatusBadRequest, err.Error())
		default:
			writeForgeError(w, nethttp.StatusInternalServerError, err.Error())
		}
		return
	}

	writeJSON(w, nethttp.StatusOK, s.instanceWithConsoleURL(toSDKInstance(row), r))
}

func (s *Server) handlePatchInstance(w nethttp.ResponseWriter, r *nethttp.Request) {
	id := r.PathValue("instanceId")

	r.Body = nethttp.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	var req forgesdk.InstanceUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeForgeError(w, nethttp.StatusBadRequest, "invalid request body")
		return
	}

	var ipxeScript *string
	if req.HasIpxeScript() {
		v := req.GetIpxeScript()
		ipxeScript = &v
	}
	var userData *string
	if req.HasUserData() {
		v := req.GetUserData()
		userData = &v
	}

	row, err := s.store.UpdateInstance(r.Context(), id, state.UpdateInstanceInput{
		Labels:        req.GetLabels(),
		TriggerReboot: req.GetTriggerReboot(),
		IpxeScript:    ipxeScript,
		UserData:      userData,
	})
	if err != nil {
		switch {
		case errors.Is(err, state.ErrNotFound):
			writeForgeError(w, nethttp.StatusNotFound, "Could not find Instance with specified ID")
		case errors.Is(err, state.ErrBadRequest):
			writeForgeError(w, nethttp.StatusBadRequest, err.Error())
		default:
			writeForgeError(w, nethttp.StatusInternalServerError, err.Error())
		}
		return
	}

	writeJSON(w, nethttp.StatusOK, s.instanceWithConsoleURL(toSDKInstance(row), r))
}

func (s *Server) handleDeleteInstance(w nethttp.ResponseWriter, r *nethttp.Request) {
	id := r.PathValue("instanceId")

	found, err := s.store.MarkInstanceForDelete(r.Context(), id)
	if err != nil {
		writeForgeError(w, nethttp.StatusInternalServerError, err.Error())
		return
	}
	if !found {
		writeForgeError(w, nethttp.StatusNotFound, "Could not find Instance with specified ID")
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]string{"status": "accepted"})
}
