// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodepools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/NVIDIA/nke/cli/internal/util"
)

// supportedFields is the subset of UpdateNodePoolRequest this command can
// change. UpdateNodePoolRequest carries more, and CWE accepts them, but only
// what is declared here is supported through nkectl today.
//
// The patch document is validated against this rather than against the full
// request type, which is what rejects an unsupported field. Supporting another
// field is declaring it here and listing it in settableFields.
type supportedFields struct {
	DesiredNodeCount *int `json:"desiredNodeCount,omitempty"`
}

// settableFields lists the fields a patch document may carry, for the
// command's help and for the error an unsupported field produces.
func settableFields() []string {
	return []string{"desiredNodeCount"}
}

// loadPatchNodePoolRequest reads the patch from the inline document or the
// named file ("-" reads stdin) and returns it as the JSON body to send.
func loadPatchNodePoolRequest(cmd *cobra.Command, patch, patchFile string) ([]byte, error) {
	raw := []byte(patch)
	if patch == "" {
		var r io.Reader
		if patchFile == "-" {
			r = cmd.InOrStdin()
		} else {
			f, err := os.Open(patchFile)
			if err != nil {
				return nil, fmt.Errorf("opening %q: %w", patchFile, err)
			}
			defer f.Close()
			r = f
		}
		data, err := io.ReadAll(r)
		if err != nil {
			return nil, fmt.Errorf("reading patch: %w", err)
		}
		raw = data
	}

	patchMap, err := util.YAMLToGenericMap(raw)
	if err != nil {
		return nil, err
	}
	if len(patchMap) == 0 {
		return nil, fmt.Errorf("nothing to patch: the patch document is empty")
	}
	// CWE declares no field of the request nullable, so a null would come back
	// as a validator message naming a JSON pointer. Refuse it here instead.
	for field, value := range patchMap {
		if value == nil {
			return nil, fmt.Errorf(
				"%s: null is not a value; omit the field to leave it unchanged", field)
		}
	}
	data, err := json.Marshal(patchMap)
	if err != nil {
		return nil, fmt.Errorf("encoding patch: %w", err)
	}

	var validate supportedFields
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&validate); err != nil {
		return nil, fmt.Errorf("validating patch: %w; nkectl can set %s",
			err, strings.Join(settableFields(), ", "))
	}
	return data, nil
}
