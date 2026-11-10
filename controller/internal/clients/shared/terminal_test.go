// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shared

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTerminal(t *testing.T) {
	sentinel := errors.New("verification failed")
	wrapped := fmt.Errorf("creating machine: %w", Terminal("nico.ErrImageVerificationFailed", fmt.Errorf("%w: image x", sentinel)))

	term, ok := AsTerminal(wrapped)
	require.True(t, ok, "the marker must be found anywhere in the chain")
	assert.Equal(t, "nico.ErrImageVerificationFailed", term.Code())

	require.ErrorIs(t, wrapped, sentinel, "errors.Is must keep working through the marker")
	assert.Equal(t, "creating machine: verification failed: image x", wrapped.Error(), "the marker adds semantics, not message noise")

	_, ok = AsTerminal(errors.New("connection refused"))
	assert.False(t, ok, "unmarked errors are not terminal")

	assert.NoError(t, Terminal("any", nil), "wrapping nil stays nil so call sites can wrap unconditionally")
}
