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
	const rotationNote = "a rotation that fails after prepare keeps the lock until its workflow is reset and completes " +
		"(see controller/docs/etcd-encryption-key-rotation.md#phase-aware-recovery)"
	tests := []struct {
		name         string
		op           db.OperationType
		message      string
		want         string
		wantRetained bool
		wantRotation bool
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
		{
			name:         "etcd key rotation points to phase-aware recovery",
			op:           db.OperationTypeEtcdKeyRotation,
			message:      "rotation failed",
			want:         "cluster update rejected: etcd_key_rotation in progress since 2026-04-23T12:00:00Z (rotation failed); " + rotationNote,
			wantRotation: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := &updateInProgressError{op: tt.op, startedAt: started, message: tt.message}
			assert.Equal(t, tt.want, err.Error())
			assert.Equal(t, tt.wantRetained, strings.Contains(err.Error(), retainedNote))
			assert.Equal(t, tt.wantRotation, strings.Contains(err.Error(), rotationNote))
		})
	}
}
