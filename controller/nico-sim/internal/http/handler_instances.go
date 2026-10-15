// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package http

import (
	"encoding/json"
	"errors"
	nethttp "net/http"
	"strings"

	nicosdk "github.com/NVIDIA/infra-controller/rest-api/sdk/standard"

	"github.com/dsx-ai-factory/nvke/controller/nico-sim/internal/state"
)

func (s *Server) handleGetInstance(w nethttp.ResponseWriter, r *nethttp.Request) {
	instanceID := r.PathValue("instanceId")
	record, backend, err := s.ownedInstance(r, instanceID)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	instance, err := backend.ConvertToAPI(r, record)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, instance)
}

func (s *Server) handleListInstances(w nethttp.ResponseWriter, r *nethttp.Request) {
	orderBy := strings.TrimSpace(r.URL.Query().Get("orderBy"))
	if orderBy != "" && !strings.EqualFold(orderBy, "CREATED_ASC") {
		writeForgeError(w, nethttp.StatusBadRequest, "unsupported orderBy; only CREATED_ASC is supported")
		return
	}

	tenant, err := s.store.GetTenantByOrg(r.Context(), orgIDFromRequest(r))
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			writeJSON(w, nethttp.StatusOK, []nicosdk.Instance{})
			return
		}
		writeForgeError(w, nethttp.StatusInternalServerError, err.Error())
		return
	}
	filter := state.ListInstancesFilter{
		SiteID:   r.URL.Query().Get("siteId"),
		TenantID: r.URL.Query().Get("tenantId"),
		Name:     r.URL.Query().Get("name"),
		VPCID:    r.URL.Query().Get("vpcId"),
	}
	if filter.TenantID != "" && filter.TenantID != tenant.ID {
		writeJSON(w, nethttp.StatusOK, []nicosdk.Instance{})
		return
	}
	filter.TenantID = tenant.ID

	if filter.SiteID != "" {
		if _, err := s.store.GetSiteForOrg(r.Context(), orgIDFromRequest(r), filter.SiteID); err != nil {
			if errors.Is(err, state.ErrNotFound) {
				writeJSON(w, nethttp.StatusOK, []nicosdk.Instance{})
				return
			}
			writeBackendError(w, err)
			return
		}
	}
	records, err := s.store.ListInstances(r.Context(), filter, parsePage(r))
	if err != nil {
		writeBackendError(w, err)
		return
	}
	instances := make([]nicosdk.Instance, 0, len(records))
	for _, record := range records {
		backend, err := s.backendForInstance(record)
		if err != nil {
			writeBackendError(w, err)
			return
		}
		instance, err := backend.ConvertToAPI(r, record)
		if err != nil {
			writeBackendError(w, err)
			return
		}
		instances = append(instances, instance)
	}
	writeJSON(w, nethttp.StatusOK, instances)
}

func (s *Server) handleCreateInstance(w nethttp.ResponseWriter, r *nethttp.Request) {
	r.Body = nethttp.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	var req nicosdk.InstanceCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeForgeError(w, nethttp.StatusBadRequest, "invalid request body")
		return
	}

	site, err := s.resolveCreateSite(r.Context(), orgIDFromRequest(r), req)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	backend, err := s.backendFor(site)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	record, err := backend.Create(r.Context(), site, req)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	instance, err := backend.ConvertToAPI(r, record)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, instance)
}

func (s *Server) handlePatchInstance(w nethttp.ResponseWriter, r *nethttp.Request) {
	instanceID := r.PathValue("instanceId")
	record, backend, err := s.ownedInstance(r, instanceID)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	r.Body = nethttp.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	var req nicosdk.InstanceUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeForgeError(w, nethttp.StatusBadRequest, "invalid request body")
		return
	}
	record, err = backend.Update(r.Context(), record, req)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	instance, err := backend.ConvertToAPI(r, record)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, instance)
}

func (s *Server) handleDeleteInstance(w nethttp.ResponseWriter, r *nethttp.Request) {
	instanceID := r.PathValue("instanceId")
	record, backend, err := s.ownedInstance(r, instanceID)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	var req nicosdk.InstanceDeleteRequest
	if r.Body != nil && r.ContentLength != 0 {
		r.Body = nethttp.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeForgeError(w, nethttp.StatusBadRequest, "invalid request body")
			return
		}
	}
	if err := backend.Delete(r.Context(), record, &req); err != nil {
		writeBackendError(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]string{"status": "accepted"})
}
