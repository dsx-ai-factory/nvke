// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package http

import (
	"crypto/sha256"
	"errors"
	"fmt"
	nethttp "net/http"

	nicosdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"

	"github.com/dsx-ai-factory/nvke/controller/nico-sim/internal/state"
)

func (s *Server) handleGetMachine(w nethttp.ResponseWriter, r *nethttp.Request) {
	tenant, err := s.store.GetTenantByOrg(r.Context(), orgIDFromRequest(r))
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			writeForgeError(w, nethttp.StatusNotFound, "Could not find Machine with specified ID")
			return
		}
		writeBackendError(w, err)
		return
	}

	machineID := r.PathValue("machineId")
	const pageSize = 100
	for page := 1; ; page++ {
		records, err := s.store.ListInstances(r.Context(), state.ListInstancesFilter{TenantID: tenant.ID}, state.Page{Number: page, Size: pageSize})
		if err != nil {
			writeBackendError(w, err)
			return
		}
		for _, record := range records {
			recordMachineID := record.MachineID
			if recordMachineID == "" && record.Backend == state.SiteBackendContainer {
				recordMachineID = "machine-" + record.ID[:8]
			}
			if recordMachineID != machineID {
				continue
			}
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
			if instance.GetMachineId() == machineID {
				writeJSON(w, nethttp.StatusOK, simulatedMachine(instance))
				return
			}
		}
		if len(records) < pageSize {
			break
		}
	}

	writeForgeError(w, nethttp.StatusNotFound, "Could not find Machine with specified ID")
}

func simulatedMachine(instance nicosdk.Instance) nicosdk.Machine {
	machine := nicosdk.NewMachine()
	machine.SetId(instance.GetMachineId())
	machine.SetInstanceId(instance.GetId())
	machine.SetTenantId(instance.GetTenantId())
	machine.SetSiteId(instance.GetSiteId())
	machine.SetInstanceTypeId(instance.GetInstanceTypeId())

	interfaces := make([]nicosdk.MachineInterface, 0, len(instance.GetInterfaces()))
	for index, instanceInterface := range instance.GetInterfaces() {
		machineInterface := nicosdk.NewMachineInterface()
		machineInterface.SetId(instanceInterface.GetId())
		machineInterface.SetMachineId(instance.GetMachineId())
		machineInterface.SetIsPrimary(index == 0)
		machineInterface.SetIpAddresses(instanceInterface.GetIpAddresses())
		if index == 0 {
			machineInterface.SetAttachedDpuMachineID(simulatedDPUMachineID(instanceInterface.GetIpAddresses()))
		}
		interfaces = append(interfaces, *machineInterface)
	}
	machine.SetMachineInterfaces(interfaces)
	return *machine
}

func simulatedDPUMachineID(ipAddresses []string) string {
	if len(ipAddresses) == 0 {
		return ""
	}
	digest := sha256.Sum256([]byte(ipAddresses[0]))
	return fmt.Sprintf("host-%x", digest[:8])
}
