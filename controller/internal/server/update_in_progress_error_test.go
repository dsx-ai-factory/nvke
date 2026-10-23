// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func TestUpdateInProgressError_Error(t *testing.T) {
	started := time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC)
	const retainedNote = "a failed certificate renewal can retain the lock until an operator clears it (see controller/docs/certificate-renewal.md)"
	tests := []struct {
		name         string
		op           db.OperationType
		message      string
		want         string
		wantRetained bool
	}{
		{
			name:    "k8s upgrade has no retained-lock note",
			op:      db.OperationTypeK8sUpgrade,
			message: "k8s upgrade in flight",
			want:    "cluster update rejected: k8s_upgrade in progress since 2026-04-23T12:00:00Z (k8s upgrade in flight)",
		},
		{
			name:         "certificate renewal notes the retained lock",
			op:           db.OperationTypeCertRenewal,
			message:      "renewal failed",
			want:         "cluster update rejected: certificate_renewal in progress since 2026-04-23T12:00:00Z (renewal failed); " + retainedNote,
			wantRetained: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := &updateInProgressError{op: tt.op, startedAt: started, message: tt.message}
			assert.Equal(t, tt.want, err.Error())
			assert.Equal(t, tt.wantRetained, strings.Contains(err.Error(), retainedNote))
		})
	}
}
