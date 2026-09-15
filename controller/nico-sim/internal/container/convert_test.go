// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package container

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestInstanceStatusForContainer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                    string
		running                 bool
		requireTPMEKCertificate bool
		tpmEKCertificate        string
		want                    string
	}{
		{
			name:                    "stopped container is an error",
			requireTPMEKCertificate: true,
			want:                    "Error",
		},
		{
			name:                    "TPM container waits for EK certificate",
			running:                 true,
			requireTPMEKCertificate: true,
			want:                    "Pending",
		},
		{
			name:                    "TPM container is ready with EK certificate",
			running:                 true,
			requireTPMEKCertificate: true,
			tpmEKCertificate:        "certificate",
			want:                    "Ready",
		},
		{
			name:    "join-token container does not require EK certificate",
			running: true,
			want:    "Ready",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := instanceStatusForContainer(
				test.running,
				test.requireTPMEKCertificate,
				test.tpmEKCertificate,
			)
			assert.Equal(t, test.want, string(got))
		})
	}
}
