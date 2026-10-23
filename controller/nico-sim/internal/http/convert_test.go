// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package http

import (
	"testing"

	"github.com/dsx-ai-factory/nvke/controller/nico-sim/internal/state"
)

func TestToSDKSubnetSeparatesIPv4PrefixAndLength(t *testing.T) {
	subnet := toSDKSubnet(state.SubnetRow{
		CIDR:         "192.168.200.0/24",
		PrefixLength: 24,
	})

	if got := subnet.GetIpv4Prefix(); got != "192.168.200.0" {
		t.Fatalf("IPv4 prefix = %q, want %q", got, "192.168.200.0")
	}
	if got := subnet.GetPrefixLength(); got != 24 {
		t.Fatalf("prefix length = %d, want 24", got)
	}
}

func TestToSDKInstanceSanitizesPreservedFailureDuringDeletion(t *testing.T) {
	instance := toSDKInstance(state.Instance{
		Status: "deleting",
		VM: &state.VMState{
			ErrorReason: "vm create/start failed: sensitive runtime detail",
		},
	})

	if got := instance.GetStatus(); got != "Terminating" {
		t.Fatalf("instance status = %q, want %q", got, "Terminating")
	}
	history := instance.GetStatusHistory()
	if len(history) != 1 {
		t.Fatalf("status history length = %d, want 1", len(history))
	}
	if got := history[0].GetStatus(); got != "Error" {
		t.Fatalf("history status = %q, want %q", got, "Error")
	}
	if got := history[0].GetMessage(); got != clientSafeInstanceError {
		t.Fatalf("history message = %q, want %q", got, clientSafeInstanceError)
	}
}

func TestToSDKInstanceKeepsNormalDeletionStatus(t *testing.T) {
	instance := toSDKInstance(state.Instance{
		Status: "deleting",
		VM:     &state.VMState{},
	})

	if got := instance.GetStatus(); got != "Terminating" {
		t.Fatalf("instance status = %q, want %q", got, "Terminating")
	}
	if history := instance.GetStatusHistory(); len(history) != 0 {
		t.Fatalf("status history = %+v, want empty", history)
	}
}
