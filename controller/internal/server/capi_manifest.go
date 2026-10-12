// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"fmt"

	"github.com/dsx-ai-factory/nvke/controller/internal/capi"
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
