// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package http

import (
	nethttp "net/http"

	"github.com/dsx-ai-factory/nvke/controller/nico-sim/internal/apierr"
	"github.com/dsx-ai-factory/nvke/controller/nico-sim/internal/state"
)

// Sim-only endpoints live under /sim. They have no NICo counterpart and exist
// for developer tooling (nico-simctl, scripts/vms.sh).

// simVM is nico-sim's own view of a VM, including states the NICo API cannot
// express (stopped).
type simVM struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Labels        map[string]string `json:"labels"`
	ObservedState string            `json:"observedState"`
	IP            string            `json:"ip"`
}

func (s *Server) handleSimListVMs(w nethttp.ResponseWriter, r *nethttp.Request) {
	records, err := s.store.ListInstances(r.Context(), state.ListInstancesFilter{Backend: state.SiteBackendVM}, state.Page{Number: 1, Size: 1000})
	if err != nil {
		writeBackendError(w, err)
		return
	}
	out := make([]simVM, 0, len(records))
	for _, rec := range records {
		labels, _ := rec.Labels()
		item := simVM{ID: rec.ID, Name: rec.Name, Labels: labels, ObservedState: rec.VM.ObservedState}
		if len(rec.Interfaces) > 0 {
			ips, _ := rec.Interfaces[0].IPAddresses()
			if len(ips) > 0 {
				item.IP = ips[0]
			}
		}
		out = append(out, item)
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (s *Server) handleSimStopVM(w nethttp.ResponseWriter, r *nethttp.Request) {
	s.setSimDesiredState(w, r, state.DesiredStateStopped)
}

func (s *Server) handleSimStartVM(w nethttp.ResponseWriter, r *nethttp.Request) {
	s.setSimDesiredState(w, r, state.DesiredStatePresent)
}

func (s *Server) setSimDesiredState(w nethttp.ResponseWriter, r *nethttp.Request, desired string) {
	found, err := s.store.SetVMDesiredState(r.Context(), r.PathValue("instanceId"), desired)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	if !found {
		writeBackendError(w, apierr.ErrNotFound)
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]string{"status": "accepted"})
}
