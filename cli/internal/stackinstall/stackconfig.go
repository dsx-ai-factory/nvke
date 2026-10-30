// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package stackinstall

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// StackConfig is the stack config file: the deployment choices that are not
// in the site spec. Mirror is a registry host[:port][/prefix] that serves the
// spire, postgresql and temporal images. ClientNetworks are the CIDRs
// admitted to the NVKE API. Credentials names where the credentials are.
type StackConfig struct {
	Mirror         string            `json:"mirror"`
	ClientNetworks []string          `json:"clientNetworks"`
	Credentials    CredentialSources `json:"credentials"`
}

// CredentialSources names where the credentials are read from. The node user
// is the only value in it; the rest are paths of files that hold one value
// each.
type CredentialSources struct {
	// DatabasePasswordFile is read on the site. The seed generates its password.
	DatabasePasswordFile string `json:"databasePasswordFile"`
	NicoTokenFile        string `json:"nicoTokenFile"`
	NodeUser             string `json:"nodeUser"`
	NodePasswordFile     string `json:"nodePasswordFile"`
	// SpireCACertFile and SpireCAKeyFile are read when the site spec sets
	// spire.upstreamCA, and must be unset otherwise.
	SpireCACertFile string `json:"spireCACertFile"`
	SpireCAKeyFile  string `json:"spireCAKeyFile"`
}

// LoadStackConfig reads the JSON object at path. A key the schema does not
// have is refused, so a typo cannot pass as an unset value.
func LoadStackConfig(path string) (StackConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return StackConfig{}, err
	}
	var c StackConfig
	if len(bytes.TrimSpace(data)) == 0 {
		return StackConfig{}, errors.New("stack config: the file is empty")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		if strings.Contains(err.Error(), `unknown field "images"`) {
			return StackConfig{}, errors.New("stack config: images is not a key; images come only from the artifact file")
		}
		return StackConfig{}, fmt.Errorf("stack config: %w", err)
	}
	if _, err := dec.Token(); err == nil {
		return StackConfig{}, errors.New("stack config: the file has content after the JSON object")
	}
	return c, nil
}
