// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package api

import (
	"testing"
	"time"
)

func TestParseBearerTokenExpiration(t *testing.T) {
	tests := []struct {
		name    string
		token   string
		want    time.Time
		wantErr bool
	}{
		{
			name:    "valid token",
			token:   "eyJraWQiOiJFUkNPOklCWFY6TjY2SDpOUEgyOjNMRlQ6SENVVToyRkFTOkJJTkw6WkxKRDpNWk9ZOkRVN0o6TVlVWSIsImFsZyI6IlJTMjU2In0.eyJzdWIiOiJjcGc4anVrN2k1dmZkODlzNHRrb3J0MmpzbCIsImF1ZCI6Im5nYyIsImFjY2VzcyI6W3sidHlwZSI6Imdyb3VwL25nYyIsIm5hbWUiOiJub3Z4d241NTJzc2MiLCJhY3Rpb25zIjpbImZvcmdlX3RlbmFudF9hZG1pbiIsInJlZ2lzdHJ5LXJlYWQiXSwibWV0YWRhdGEiOm51bGx9XSwiaXNzIjoiYXV0aG4ubnZpZGlhLmNvbSIsIm9wdGlvbnMiOltdLCJleHAiOjE3NTY0OTU3NjYsImlhdCI6MTc1NjQ5NTE2NiwianRpIjoiNjgyNjI0YTItMzQxOC00NTBlLWIxM2MtNjg4MmUyMzQzOTNkIn0.sFSCYC9IFarvy66AMV9hG6CdsZ-w1KVcDa0Z2zFNX4yknd18ViMXsul-IxhI266XtOKRK9ynP9wyuIp7Yo__7HlBkS1BRiWkwNgxPKqnN8q4niC3m-srAUJ3NdgTjCzugKFU_tyHd8mxyigUq7Y1l2VyiCXuZwrBK04fyT0V0yh32TLFXc1wk-q9pOoTdwOnk-9QxVIaSZdi--KPKf6DCLeJebOJNbaCaiQRpTaTMRzpqZFKLtbzAiHc5IsnRxQw-RuShX2OvErsiHN3SSneZhpwvqxoobtTZqEMqmoI8T_0jNUcf53ImVsJjQvYvXZfXhSB_eynpf9EPbJiwLpTQmgGnItLmK_ST1g1IvZdtV-kY3oLfcx1dfpCnyrBjKq991HpQFL8nbH5b7gxU9EJVaH8VUtm6XyzWBJDiPthi9zXfXpaCbQ67agehSgg2cxN0l7-vfzaVK-CG8LIJ87r_mL8RBE1VC42wE1c4URlXeB0HGhy8WTALCxROOwvb8bmR_vxd7E7sSfjDo7z0dSnzvT7BYAwyQw6_AMzg55mU2Ob2ce_B-GjbfQP71P3r4lNrrmjA6v3_DwJQ1uAMcAxG4hcP_l7JlAkpftuOXoX6Okr2U1GESsAD_TF-6XbIy6naZBEL0UZxW1iiNgGmQNmUTAT5uypwO5sPsg2zDQsRiU",
			want:    time.Unix(1756495766, 0),
			wantErr: false,
		},
		{
			name:    "valid token",
			token:   "eyJraWQiOiJFUkNPOklCWFY6TjY2SDpOUEgyOjNMRlQ6SENVVToyRkFTOkJJTkw6WkxKRDpNWk9ZOkRVN0o6TVlVWSIsImFsZyI6IlJTMjU2In0.eyJzdWIiOiJjcGc4anVrN2k1dmZkODlzNHRrb3J0MmpzbCIsImF1ZCI6Im5nYyIsImFjY2VzcyI6W3sidHlwZSI6Imdyb3VwL25nYyIsIm5hbWUiOiJub3Z4d241NTJzc2MiLCJhY3Rpb25zIjpbImZvcmdlX3RlbmFudF9hZG1pbiIsInJlZ2lzdHJ5LXJlYWQiXSwibWV0YWRhdGEiOm51bGx9XSwiaXNzIjoiYXV0aG4ubnZpZGlhLmNvbSIsIm9wdGlvbnMiOltdLCJleHAiOjE3NTY0OTU3OTYsImlhdCI6MTc1NjQ5NTE5NiwianRpIjoiYmVhMzE5NWUtODZiNy00MjY3LWI3ZjktZjIzN2JhODRmNDNkIn0.yn-nfNVr2Hld9x7soUNoV38qxMhssm-ZcwYW2xwJLwMZYLbDjX-TI9AQNFvaMjNu11kcsKBOpXoPUT59MHiOlmlMPy38osBV8IWHPwL1xX4uTwHaOQy4CdcHqDEGrO6rXhpYMGJGwaOBjNjzZ9KLnVD8Kh4S9A8tCn_0Yt5WhhzXK6KuzjsOmjCK_Ng9oKGESb-iTd0qG62MeKJlazfdSoxnzhK_n9gsKPCLf9lYQHiMkUl0i_dQKcmWcN1HZ5_K7tyj2CqYbZtigqs--wG6FadUc4sKKCFyBnoTV4a1q2UanmYX4ot5pCiYJsBZDKgBRhyi7AgvMivr-_FdXbHMcdFf5q3XL2xitEazUnNjugIRpMPEmtI8_xWYXibMjtvmGsBB9QQ31nty_kKB48teks9I-SuK8N_ceOvfx5n3Jl8YHdA_3pr2PFu_boyzlLfET0z0Ly2CWDzXGIYd121mESYrZE08osWdgPSEVb0MEASnFt4_r-3wdM6ZE7_ocJl4l1VWY6YkD_QTgMhrSqr5ngQl5wjrnheMt5mQxAIJ0UrcWgJi4SAxwq8xzqu3nP0Fdt23iBjIq_Z7hm9QTj2wWBZ2LIP10TZha-jd6xnG8BwAw61PHK5M8mu4EdH2hVeXsUX6ItY0hSCh3CKohIMXwCVIgUFQLKCk4SVSx8ZKiuY",
			want:    time.Unix(1756495796, 0),
			wantErr: false,
		},
		{
			name:    "valid token no signature",
			token:   "eyJraWQiOiJFUkNPOklCWFY6TjY2SDpOUEgyOjNMRlQ6SENVVToyRkFTOkJJTkw6WkxKRDpNWk9ZOkRVN0o6TVlVWSIsImFsZyI6IlJTMjU2In0.eyJzdWIiOiJjcGc4anVrN2k1dmZkODlzNHRrb3J0MmpzbCIsImF1ZCI6Im5nYyIsImFjY2VzcyI6W3sidHlwZSI6Imdyb3VwL25nYyIsIm5hbWUiOiJub3Z4d241NTJzc2MiLCJhY3Rpb25zIjpbImZvcmdlX3RlbmFudF9hZG1pbiIsInJlZ2lzdHJ5LXJlYWQiXSwibWV0YWRhdGEiOm51bGx9XSwiaXNzIjoiYXV0aG4ubnZpZGlhLmNvbSIsIm9wdGlvbnMiOltdLCJleHAiOjE3NTY0OTU3OTYsImlhdCI6MTc1NjQ5NTE5NiwianRpIjoiYmVhMzE5NWUtODZiNy00MjY3LWI3ZjktZjIzN2JhODRmNDNkIn0.",
			want:    time.Unix(1756495796, 0),
			wantErr: false,
		},
		{
			name:    "valid token no expiration",
			token:   "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIiwiYWRtaW4iOnRydWUsImlhdCI6MTUxNjIzOTAyMn0.KMUFsIDTnFmyG3nMiGM6H9FNFUROf3wh7SmqJp-QV30",
			want:    time.Time{},
			wantErr: true,
		},
		{
			name:    "invalid token",
			token:   "foobar",
			want:    time.Time{},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseBearerTokenExpiration(tt.token)

			if (err != nil) != tt.wantErr {
				t.Errorf("parseBearerTokenExpiration() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !got.Equal(tt.want) {
				t.Errorf("parseBearerTokenExpiration() = %v, want %v", got, tt.want)
			}
		})
	}
}
