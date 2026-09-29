// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDBNameFromDSN(t *testing.T) {
	tests := []struct {
		name string
		dsn  string
		want string
	}{
		{
			name: "legacy default",
			dsn:  "postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable",
			want: "postgres",
		},
		{
			name: "greenfield-named database",
			dsn:  "postgres://cloud:secret@cnpg-cloud-rw.cnpg-cloud.svc.cluster.local/nke-controller?sslmode=disable",
			want: "nke-controller",
		},
		{
			name: "legacy database name",
			dsn:  "postgres://cloud:secret@cnpg-cloud-rw.cnpg-cloud.svc.cluster.local/cloud-workflow?sslmode=disable",
			want: "cloud-workflow",
		},
		{
			name: "no path",
			dsn:  "postgres://cloud:secret@cnpg-cloud-rw.cnpg-cloud.svc.cluster.local:5432",
			want: "",
		},
		{
			name: "unparseable DSN",
			dsn:  "://not a url",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, dbNameFromDSN(tt.dsn))
		})
	}
}
