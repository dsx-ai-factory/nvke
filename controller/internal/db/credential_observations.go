// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"time"
)

// CertificateObservation contains only bounded public certificate metadata.
type CertificateObservation struct {
	CertificateSHA256 string    `json:"certificateSHA256,omitempty"`
	Credential        string    `json:"credential"`
	State             string    `json:"state"`
	NotAfter          time.Time `json:"notAfter,omitempty"`
}

// CertificateInspection records the latest attempt separately from the last successful response.
// A failed probe preserves known expiration dates but makes their availability false.
type CertificateInspection struct {
	AttemptedAt  time.Time                `json:"attemptedAt"`
	ObservedAt   *time.Time               `json:"observedAt,omitempty"`
	Available    bool                     `json:"available"`
	Reason       string                   `json:"reason,omitempty"`
	Certificates []CertificateObservation `json:"certificates,omitempty"`
	// Retries counts consecutive attempts that were failed, or found a required credential
	// unavailable, and so are repeated sooner than the inspection interval. Zero otherwise.
	Retries int `json:"retries,omitempty"`
}

// CredentialCSRObservation is a cluster aggregate; CSR names and requester identities are never retained.
type CredentialCSRObservation struct {
	ObservedAt      time.Time  `json:"observedAt"`
	Pending         int        `json:"pending"`
	Failed          int        `json:"failed"`
	Denied          int        `json:"denied"`
	OldestPendingAt *time.Time `json:"oldestPendingAt,omitempty"`
}
