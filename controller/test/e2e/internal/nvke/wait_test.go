// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvke

import (
	"testing"
	"time"

	"github.com/dsx-ai-factory/nvke/controller/api"
)

// A stall window at or above the budget can never elapse first, which is how
// the short lanes keep the behavior they had: the deadline ends them and
// nothing else does. The bare-metal lanes are the ones long enough for a
// silence to mean something.
func TestStallWindow(t *testing.T) {
	for name, tc := range map[string]struct {
		budget time.Duration
		want   time.Duration
		fires  bool
	}{
		"forge tenant":    {budget: 45 * time.Minute, want: 15 * time.Minute, fires: true},
		"long container":  {budget: 30 * time.Minute, want: 15 * time.Minute, fires: true},
		"two hours":       {budget: 2 * time.Hour, want: 40 * time.Minute, fires: true},
		"at the floor":    {budget: 15 * time.Minute, want: 15 * time.Minute, fires: false},
		"short simulator": {budget: 10 * time.Minute, want: 15 * time.Minute, fires: false},
		"shortest of all": {budget: 5 * time.Minute, want: 15 * time.Minute, fires: false},
	} {
		t.Run(name, func(t *testing.T) {
			got := stallWindow(tc.budget)
			if got != tc.want {
				t.Errorf("stallWindow(%v) = %v, want %v", tc.budget, got, tc.want)
			}
			if fires := got < tc.budget; fires != tc.fires {
				t.Errorf("stallWindow(%v) = %v fires=%v, want fires=%v", tc.budget, got, fires, tc.fires)
			}
		})
	}
}

// Run 37882425829: the cluster message never changed while the amd64 control
// plane gained an address and then a reachable agent, and the wait called it
// stuck. Each of those has to count as progress; a retry that only bumps the
// node's attempt count must not, or a node retrying forever never stalls.
func TestProvisioningProgress(t *testing.T) {
	const waiting = "Waiting for a control plane node to be ready to join"
	node := func(ip string, agent api.ConditionStatus, provisioning string) api.Node {
		var n api.Node
		n.Name, n.IP = "cp", ip
		n.Conditions.NodeAgentReady.Status = agent
		n.Conditions.Provisioning.Message = provisioning
		return n
	}
	worker := api.Node{Name: "w"}

	booting := provisioningProgress(waiting, []api.Node{node("", api.ConditionStatusFalse, "")})
	addressed := provisioningProgress(waiting, []api.Node{node("192.0.2.10", api.ConditionStatusFalse, "7 attempts have failed so far")})
	retried := provisioningProgress(waiting, []api.Node{node("192.0.2.10", api.ConditionStatusFalse, "8 attempts have failed so far")})
	reachable := provisioningProgress(waiting, []api.Node{node("192.0.2.10", api.ConditionStatusTrue, "8 attempts have failed so far")})

	if addressed == booting {
		t.Error("a node gaining an address is progress")
	}
	if reachable == addressed {
		t.Error("a node's agent becoming reachable is progress")
	}
	if retried != addressed {
		t.Error("a retry that only changes the attempt count is not progress")
	}
	if provisioningProgress("Provisioning control plane", nil) == provisioningProgress(waiting, nil) {
		t.Error("a new cluster message is progress")
	}

	cp := node("192.0.2.10", api.ConditionStatusTrue, "")
	if provisioningProgress(waiting, []api.Node{cp, worker}) != provisioningProgress(waiting, []api.Node{worker, cp}) {
		t.Error("the order the API lists nodes in is not progress")
	}
}
