// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package health contains the shared management-plane probe assertions.
package health

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Check requires a successful response and, for /api/version, the expected version.
func Check(path string, status int, body []byte, expected string) error {
	if status != 200 {
		return fmt.Errorf("%s returned HTTP %d: %s", path, status, body)
	}
	switch path {
	case "/healthz", "/readyz":
		return nil
	case "/api/version":
		if expected == "" {
			return errors.New("expected version is required")
		}
		var response struct {
			Version string `json:"version"`
		}
		if err := json.Unmarshal(body, &response); err != nil {
			return err
		}
		if response.Version != expected {
			return fmt.Errorf("controller version %q, expected %q", response.Version, expected)
		}
		return nil
	default:
		return fmt.Errorf("unsupported health path %q", path)
	}
}
