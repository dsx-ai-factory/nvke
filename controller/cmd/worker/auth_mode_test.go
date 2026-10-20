// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/dsx-ai-factory/nvke/auth/authn"
)

func TestAuthMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		authenticator *authn.Authenticator
		permissive    bool
		want          string
	}{
		{name: "no authenticator is off", want: "off"},
		{name: "no authenticator is off even when permissiveMode is set", permissive: true, want: "off"},
		{name: "an authenticator with permissiveMode is permissive", authenticator: &authn.Authenticator{}, permissive: true, want: "permissive"},
		{name: "an authenticator without permissiveMode is enforcing", authenticator: &authn.Authenticator{}, want: "enforcing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, authMode(tt.authenticator, tt.permissive))
		})
	}
}
