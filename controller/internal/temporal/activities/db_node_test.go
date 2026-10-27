// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

func TestDeleteNodePreReadErrors(t *testing.T) {
	readErr := errors.New("connection reset")
	tests := map[string]struct {
		getNodeErr  error
		wantErrIs   error
		wantNoError bool
	}{
		"node already deleted by a prior delivery succeeds": {
			getNodeErr:  fmt.Errorf("node: %w", db.ErrNotFound),
			wantNoError: true,
		},
		"other read errors are returned": {
			getNodeErr: readErr,
			wantErrIs:  readErr,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			nodeID := uuid.New()
			tx := &testutil.MockTx{}
			store := &testutil.MockDB{MockTx: tx}
			store.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
			tx.On("GetNode", mock.Anything, nodeID).Return(db.Node{}, tt.getNodeErr).Once()

			err := (&Activities{db: store}).DeleteNode(t.Context(), nodeID)

			if tt.wantNoError {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, tt.wantErrIs)
			}
			tx.AssertNotCalled(t, "DeleteNode", mock.Anything, mock.Anything)
			tx.AssertExpectations(t)
		})
	}
}
