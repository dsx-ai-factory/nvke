// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubeletcsr

import (
	"crypto/x509"
	"crypto/x509/pkix"

	certificatesv1 "k8s.io/api/certificates/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

type kubeletClientCSRDecisions struct {
	pending       []*certificatesv1.CertificateSigningRequest
	approved      []*certificatesv1.CertificateSigningRequest
	issued        []*certificatesv1.CertificateSigningRequest
	denied        []*certificatesv1.CertificateSigningRequest
	failed        []*certificatesv1.CertificateSigningRequest
	invalid       []*certificatesv1.CertificateSigningRequest
	invalidReason string
}

func decideKubeletClientCSRs(machine *clusterv1.Machine, csrs []certificatesv1.CertificateSigningRequest) kubeletClientCSRDecisions {
	nodeName := expectedKubeletNodeName(machine)
	var csrDecisions kubeletClientCSRDecisions
	for i := range csrs {
		csr := &csrs[i]
		switch {
		case validateKubeletClientCSR(csr, nodeName) != "":
			csrDecisions.invalid = append(csrDecisions.invalid, csr)
		case clientCSRDenied(csr):
			csrDecisions.denied = append(csrDecisions.denied, csr)
		case clientCSRFailed(csr):
			csrDecisions.failed = append(csrDecisions.failed, csr)
		case len(csr.Status.Certificate) > 0:
			csrDecisions.issued = append(csrDecisions.issued, csr)
		case clientCSRApproved(csr):
			csrDecisions.approved = append(csrDecisions.approved, csr)
		default:
			csrDecisions.pending = append(csrDecisions.pending, csr)
		}
	}
	csrDecisions.invalidReason = lastInvalidKubeletClientCSRReason(csrDecisions.invalid, nodeName)
	return csrDecisions
}

// validateKubeletClientCSR reports why csr must not be approved for nodeName.
// Usage and the system:node subject match the kubernetes.io/kube-apiserver-client-kubelet signer.
// Subject attributes and extensions are stricter, because that signer copies requested extensions
// into the issued certificate. spec.groups is the requestor's groups, not the certificate groups,
// so system:nodes is required on the certificate subject and not on the CSR object.
func validateKubeletClientCSR(csr *certificatesv1.CertificateSigningRequest, nodeName string) string {
	if !kubeletClientUsagesAllowed(csr.Spec.Usages) {
		return "spec.usages must be digital signature and client auth, with key encipherment optional"
	}

	parsed, reason := parseSingleCertificateRequest(csr.Spec.Request)
	if reason != "" {
		return reason
	}

	if err := parsed.CheckSignature(); err != nil {
		return "certificate request signature is invalid"
	}

	if reason := validateKubeletClientSubject(parsed, nodeName); reason != "" {
		return reason
	}

	if reason := validateKubeletClientExtensions(parsed.Extensions, csr.Spec.Usages); reason != "" {
		return reason
	}

	return ""
}

// kubeletClientUsagesAllowed reports whether usages are a set the kubernetes.io/kube-apiserver-client-kubelet signer will issue.
// Digital signature and client auth are required. Key encipherment is included only for an RSA key.
func kubeletClientUsagesAllowed(usages []certificatesv1.KeyUsage) bool {
	hasSignature := false
	hasClientAuth := false
	for _, usage := range usages {
		switch usage { //nolint:exhaustive // every usage other than the three below is rejected
		case certificatesv1.UsageDigitalSignature:
			hasSignature = true
		case certificatesv1.UsageClientAuth:
			hasClientAuth = true
		case certificatesv1.UsageKeyEncipherment:
			// UsageKeyEncipherment is optionally allowed.
		default:
			return false
		}
	}

	// Assert the two required usages are present.
	return hasSignature && hasClientAuth
}

func validateKubeletClientSubject(parsed *x509.CertificateRequest, nodeName string) string {
	if reason := validateKubeletNodeSubject(parsed, nodeName); reason != "" {
		return reason
	}
	// SubjectAltNames are not allowed.
	if len(parsed.DNSNames) > 0 {
		return "DNS subjectAltNames are not allowed"
	}
	if len(parsed.EmailAddresses) > 0 {
		return "Email subjectAltNames are not allowed"
	}
	if len(parsed.IPAddresses) > 0 {
		return "IP subjectAltNames are not allowed"
	}
	if len(parsed.URIs) > 0 {
		return "URI subjectAltNames are not allowed"
	}
	return ""
}

// validateKubeletClientExtensions allows key usage and extended key usage, and rejects every other extension.
// Key usage may set digital signature and key encipherment. Extended key usage must be client auth.
// A usage asserted by an extension must also be present in usages.
func validateKubeletClientExtensions(extensions []pkix.Extension, usages []certificatesv1.KeyUsage) string {
	return validateKubeletExtensions(extensions, usages, oidClientAuth, certificatesv1.UsageClientAuth, false)
}

func clientCSRApproved(csr *certificatesv1.CertificateSigningRequest) bool {
	return csrConditionTrue(csr, certificatesv1.CertificateApproved)
}

func clientCSRDenied(csr *certificatesv1.CertificateSigningRequest) bool {
	return csrConditionTrue(csr, certificatesv1.CertificateDenied)
}

func clientCSRFailed(csr *certificatesv1.CertificateSigningRequest) bool {
	return csrConditionTrue(csr, certificatesv1.CertificateFailed)
}

func lastInvalidKubeletClientCSRReason(invalid []*certificatesv1.CertificateSigningRequest, nodeName string) string {
	var latest *certificatesv1.CertificateSigningRequest
	for _, csr := range invalid {
		if latest == nil || csr.CreationTimestamp.Time.After(latest.CreationTimestamp.Time) {
			latest = csr
		}
	}
	if latest == nil {
		return ""
	}
	return latest.Name + ": " + validateKubeletClientCSR(latest, nodeName)
}
