// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/dsx-ai-factory/nke/controller/internal/capi"
)

const (
	capiInfrastructureGroup = "infrastructure.cluster.x-k8s.io"
)

func isEmptyManifestObject(obj map[string]interface{}) bool {
	return len(obj) == 0
}

func rawManifestFromObject(obj map[string]interface{}) (json.RawMessage, error) {
	if isEmptyManifestObject(obj) {
		return nil, nil
	}

	data, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal manifest object: %w", err)
	}
	return json.RawMessage(data), nil
}

func objectFromRawManifest(raw json.RawMessage) (map[string]interface{}, error) {
	if capi.IsEmptyManifest(raw) {
		return map[string]interface{}{}, nil
	}

	var obj map[string]interface{}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("failed to unmarshal manifest object: %w", err)
	}
	return obj, nil
}

// firstInfraMachineTemplateObject returns the provisioned control-plane MachineTemplate
// from the stored preference list (index 0). Extra entries are merge history and are
// not part of the API.
func firstInfraMachineTemplateObject(prefs []json.RawMessage) map[string]interface{} {
	if len(prefs) == 0 {
		return nil
	}
	obj, err := objectFromRawManifest(prefs[0])
	if err != nil || isEmptyManifestObject(obj) {
		return nil
	}
	return obj
}

// infraMachineTemplatePrefsFromAPI stores a single API MachineTemplate as the
// control-plane preference list the controller persists internally.
func infraMachineTemplatePrefsFromAPI(obj map[string]interface{}) ([]json.RawMessage, error) {
	raw, err := rawManifestFromObject(obj)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	return []json.RawMessage{raw}, nil
}

func validateCAPIInfrastructureManifest(obj map[string]interface{}) error {
	if isEmptyManifestObject(obj) {
		return nil
	}

	manifest := &unstructured.Unstructured{Object: obj}
	apiVersion := manifest.GetAPIVersion()
	if apiVersion == "" {
		return fmt.Errorf("apiVersion is required")
	}
	if manifest.GetKind() == "" {
		return fmt.Errorf("kind is required")
	}

	groupVersion, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return fmt.Errorf("apiVersion %q is invalid: %w", apiVersion, err)
	}
	if !isCAPIInfrastructureGroup(groupVersion.Group) {
		return fmt.Errorf(
			"apiVersion group %q must be a CAPI infrastructure provider group such as %q",
			groupVersion.Group,
			capiInfrastructureGroup,
		)
	}

	return nil
}

func isCAPIInfrastructureGroup(group string) bool {
	return group == capiInfrastructureGroup
}
