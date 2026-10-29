// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package launchlayer

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// ApplyNodePoolNodePayload sets an empty LaunchLayer payload on node. Unlike
// NICo's pinned instance type, nothing about a LaunchLayer node's identity
// (instanceID, machineID, rackID) is known before CAPLL creates the Compute
// instance — all three are reported later, on LaunchLayerMachine.status.
func (*Client) ApplyNodePoolNodePayload(_ db.NodePool, node *db.Node) error {
	node.SetLaunchLayer(db.LaunchLayerNodeProvider{})
	return nil
}

// ApplyPlaceholderNodePayload sets an empty LaunchLayer payload on node so
// downstream strict provider-payload readers succeed.
func (*Client) ApplyPlaceholderNodePayload(node *db.Node) {
	node.SetLaunchLayer(db.LaunchLayerNodeProvider{})
}

// ApplyObservedNodeIdentity records the instance, machine and rack IDs
// LaunchLayerMachine.status reports.
func (*Client) ApplyObservedNodeIdentity(node *db.Node, infraMachine *unstructured.Unstructured) error {
	launchLayer := db.LaunchLayerNodeProvider{}
	if node.LaunchLayer != nil {
		launchLayer = *node.LaunchLayer
	}
	for field, recorded := range map[string]*string{
		"instanceID": &launchLayer.InstanceID,
		"machineID":  &launchLayer.MachineID,
		"rackID":     &launchLayer.RackID,
	} {
		observed, _, err := unstructured.NestedString(infraMachine.Object, "status", field)
		if err != nil {
			return fmt.Errorf("read LaunchLayerMachine status.%s: %w", field, err)
		}
		if *recorded == "" {
			*recorded = observed
		}
	}
	node.SetLaunchLayer(launchLayer)
	return nil
}
