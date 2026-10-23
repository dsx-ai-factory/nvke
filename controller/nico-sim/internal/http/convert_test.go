// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package http

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	tests := []struct {
		name            string
		vm              *state.VMState
		wantHistory     bool
		wantHistoryText string
	}{
		{
			name: "preserved failure is sanitized",
			vm: &state.VMState{
				ErrorReason: "vm create/start failed: sensitive runtime detail",
			},
			wantHistory:     true,
			wantHistoryText: clientSafeInstanceError,
		},
		{
			name:        "normal deletion has no failure history",
			vm:          &state.VMState{},
			wantHistory: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instance := toSDKInstance(state.Instance{
				Status: "deleting",
				VM:     tt.vm,
			})

			assert.Equal(t, "Terminating", string(instance.GetStatus()))
			history := instance.GetStatusHistory()
			if !tt.wantHistory {
				assert.Empty(t, history)
				return
			}

			require.Len(t, history, 1)
			assert.Equal(t, "Error", history[0].GetStatus())
			assert.Equal(t, tt.wantHistoryText, history[0].GetMessage())
		})
	}
}
