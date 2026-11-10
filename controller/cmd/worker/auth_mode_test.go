// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAuthMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		on         bool
		permissive bool
		want       string
	}{
		{name: "off", want: "off"},
		{name: "off even when permissive is set", permissive: true, want: "off"},
		{name: "on and permissive", on: true, permissive: true, want: "permissive"},
		{name: "on and not permissive is enforcing", on: true, want: "enforcing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, authMode(tt.on, tt.permissive))
		})
	}
}
