// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"context"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	oidKeyUsage         = "2.5.29.15"
	oidSubjectAltName   = "2.5.29.17"
	oidBasicConstraints = "2.5.29.19"
	oidExtendedKeyUsage = "2.5.29.37"
	oidClientAuth       = "1.3.6.1.5.5.7.3.2"
)

const (
	kubeletNodeNamePrefix     = "system:node:"
	kubeletNodeGroup          = "system:nodes"
	certificateRequestPEMType = "CERTIFICATE REQUEST"
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

func parseSingleCertificateRequest(request []byte) (*x509.CertificateRequest, string) {
	block, trailing := pem.Decode(request)
	if block == nil || block.Type != certificateRequestPEMType || strings.TrimSpace(string(trailing)) != "" {
		return nil, "spec.request is not a single PEM-encoded certificate request"
	}
	parsed, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, "spec.request cannot be parsed as a certificate request"
	}
	return parsed, ""
}

func validateKubeletClientSubject(parsed *x509.CertificateRequest, nodeName string) string {
	expectedCommonName := kubeletNodeNamePrefix + nodeName
	if parsed.Subject.CommonName != expectedCommonName {
		return "subject common name is not " + expectedCommonName
	}
	if !slices.Equal(parsed.Subject.Organization, []string{kubeletNodeGroup}) {
		return "subject organization is not system:nodes"
	}
	// Names must contain exactly one common name and one organization, and extra attributes are not allowed.
	if len(parsed.Subject.Names) != 2 {
		return "subject has extra attributes"
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
	for _, extension := range extensions {
		switch extension.Id.String() {
		case oidKeyUsage:
			if reason := validateKubeletClientKeyUsage(extension, usages); reason != "" {
				return reason
			}
		case oidExtendedKeyUsage:
			if reason := validateKubeletClientExtendedKeyUsage(extension, usages); reason != "" {
				return reason
			}
		case oidSubjectAltName:
			return "subjectAltName extensions are not allowed"
		case oidBasicConstraints:
			return "certificate request must not assert CA capability"
		default:
			return "certificate request contains an unsupported extension"
		}
	}
	return ""
}

func validateKubeletClientKeyUsage(extension pkix.Extension, usages []certificatesv1.KeyUsage) string {
	const inconsistent = "key usage extension is not consistent with spec.usages"
	var bits asn1.BitString
	rest, err := asn1.Unmarshal(extension.Value, &bits)
	if err != nil || len(rest) != 0 {
		return "key usage extension is invalid"
	}
	for bit := range bits.BitLength {
		if bits.At(bit) == 0 {
			continue
		}
		switch bit {
		case 0: // digitalSignature
			if !slices.Contains(usages, certificatesv1.UsageDigitalSignature) {
				return inconsistent
			}
		case 2: // keyEncipherment
			if !slices.Contains(usages, certificatesv1.UsageKeyEncipherment) {
				return inconsistent
			}
		default:
			return inconsistent
		}
	}
	return ""
}

func validateKubeletClientExtendedKeyUsage(extension pkix.Extension, usages []certificatesv1.KeyUsage) string {
	const inconsistent = "extended key usage extension is not consistent with spec.usages"
	var purposes []asn1.ObjectIdentifier
	rest, err := asn1.Unmarshal(extension.Value, &purposes)
	if err != nil || len(rest) != 0 {
		return "extended key usage extension is invalid"
	}
	if len(purposes) != 1 || purposes[0].String() != oidClientAuth {
		return inconsistent
	}
	if !slices.Contains(usages, certificatesv1.UsageClientAuth) {
		return inconsistent
	}
	return ""
}

func clientCSRApproved(csr *certificatesv1.CertificateSigningRequest) bool {
	return clientCSRConditionTrue(csr, certificatesv1.CertificateApproved)
}

func clientCSRDenied(csr *certificatesv1.CertificateSigningRequest) bool {
	return clientCSRConditionTrue(csr, certificatesv1.CertificateDenied)
}

func clientCSRFailed(csr *certificatesv1.CertificateSigningRequest) bool {
	return clientCSRConditionTrue(csr, certificatesv1.CertificateFailed)
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

func clientCSRConditionTrue(csr *certificatesv1.CertificateSigningRequest, conditionType certificatesv1.RequestConditionType) bool {
	for _, condition := range csr.Status.Conditions {
		if condition.Type == conditionType && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func approveKubeletClientCSRs(ctx context.Context, tenantClient client.Client, machine *clusterv1.Machine, csrs []*certificatesv1.CertificateSigningRequest, logger *slog.Logger) error {
	slices.SortFunc(csrs, func(left, right *certificatesv1.CertificateSigningRequest) int {
		return strings.Compare(left.Name, right.Name)
	})
	for _, csr := range csrs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := approveKubeletClientCSR(ctx, tenantClient, csr); err != nil {
			return err
		}
		logger.Info("Approved kubelet client CSR",
			"namespace", machine.Namespace,
			"clusterName", machine.Spec.ClusterName,
			"machineName", machine.Name,
			"csrName", csr.Name,
			"nodeName", expectedKubeletNodeName(machine),
		)
	}
	return nil
}

func approveKubeletClientCSR(ctx context.Context, tenantClient client.Client, csr *certificatesv1.CertificateSigningRequest) error {
	updated := csr.DeepCopy()
	now := metav1.Now()
	updated.Status.Conditions = append(updated.Status.Conditions, certificatesv1.CertificateSigningRequestCondition{
		Type:               certificatesv1.CertificateApproved,
		Status:             corev1.ConditionTrue,
		Reason:             KubeletClientCSRApprovalReason,
		Message:            "kubelet client CSR matches the Machine bootstrap identity and node name",
		LastUpdateTime:     now,
		LastTransitionTime: now,
	})
	if err := tenantClient.SubResource("approval").Update(ctx, updated); err != nil {
		return fmt.Errorf("approving kubelet client CSR %s: %w", csr.Name, err)
	}
	return nil
}
