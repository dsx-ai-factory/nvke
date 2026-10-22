// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients/nico"
	"github.com/dsx-ai-factory/nvke/controller/internal/clients/shared"
)

// TestTerminalToNonRetryable pins the retry semantics of the terminal-error
// adapter: any client error marked permanent with shared.Terminal is a
// terminal workflow failure carrying the marker's code, while every other
// error keeps the default retry behavior. The image-verification refusal is
// the first producer; the adapter must work for it without naming it.
func TestTerminalToNonRetryable(t *testing.T) {
	refusal := fmt.Errorf("creating machine: %w",
		shared.Terminal("nico.ErrImageVerificationFailed",
			fmt.Errorf("%w: image x: no passing verdict", nico.ErrImageVerificationFailed)))
	err := terminalToNonRetryable(refusal)
	var appErr *temporal.ApplicationError
	require.ErrorAs(t, err, &appErr)
	assert.True(t, appErr.NonRetryable(), "a permanent refusal must not retry")
	assert.Equal(t, "nico.ErrImageVerificationFailed", appErr.Type(), "the marker's code is the application-error type, usable in RetryPolicy.NonRetryableErrorTypes")
	assert.Contains(t, appErr.Error(), "no passing verdict", "the named check context must survive classification")

	transient := errors.New("apply NicoMachine: connection refused")
	assert.Equal(t, transient, terminalToNonRetryable(transient), "unmarked errors keep default retry behavior")

	assert.NoError(t, terminalToNonRetryable(nil))
}
