// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubeletcsr

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
	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	oidKeyUsage         = "2.5.29.15"
	oidSubjectAltName   = "2.5.29.17"
	oidBasicConstraints = "2.5.29.19"
	oidExtendedKeyUsage = "2.5.29.37"
	oidClientAuth       = "1.3.6.1.5.5.7.3.2"
	oidServerAuth       = "1.3.6.1.5.5.7.3.1"
)

const (
	kubeletNodeNamePrefix     = "system:node:"
	kubeletNodeGroup          = "system:nodes"
	certificateRequestPEMType = "CERTIFICATE REQUEST"
)

func kubeletNodeUsername(nodeName string) string {
	return kubeletNodeNamePrefix + nodeName
}

func isKubeletNodeUsername(username string) bool {
	return strings.HasPrefix(username, kubeletNodeNamePrefix)
}

func kubeletNodeNameFromUsername(username string) string {
	return strings.TrimPrefix(username, kubeletNodeNamePrefix)
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

func validateKubeletNodeSubject(parsed *x509.CertificateRequest, nodeName string) string {
	expectedCommonName := kubeletNodeUsername(nodeName)
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
	return ""
}

func validateKubeletExtensions(extensions []pkix.Extension, usages []certificatesv1.KeyUsage, expectedExtendedUsageOID string, expectedExtendedUsage certificatesv1.KeyUsage, allowSubjectAltName bool) string {
	for _, extension := range extensions {
		switch extension.Id.String() {
		case oidKeyUsage:
			if reason := validateKeyUsageExtension(extension, usages); reason != "" {
				return reason
			}
		case oidExtendedKeyUsage:
			if reason := validateExtendedKeyUsageExtension(extension, usages, expectedExtendedUsageOID, expectedExtendedUsage); reason != "" {
				return reason
			}
		case oidSubjectAltName:
			if !allowSubjectAltName {
				return "subjectAltName extensions are not allowed"
			}
		case oidBasicConstraints:
			return "certificate request must not assert CA capability"
		default:
			return "certificate request contains an unsupported extension"
		}
	}
	return ""
}

func validateKeyUsageExtension(extension pkix.Extension, usages []certificatesv1.KeyUsage) string {
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

func validateExtendedKeyUsageExtension(extension pkix.Extension, usages []certificatesv1.KeyUsage, expectedOID string, expectedUsage certificatesv1.KeyUsage) string {
	const inconsistent = "extended key usage extension is not consistent with spec.usages"
	var purposes []asn1.ObjectIdentifier
	rest, err := asn1.Unmarshal(extension.Value, &purposes)
	if err != nil || len(rest) != 0 {
		return "extended key usage extension is invalid"
	}
	if len(purposes) != 1 || purposes[0].String() != expectedOID {
		return inconsistent
	}
	if !slices.Contains(usages, expectedUsage) {
		return inconsistent
	}
	return ""
}

func csrConditionTrue(csr *certificatesv1.CertificateSigningRequest, conditionType certificatesv1.RequestConditionType) bool {
	for _, condition := range csr.Status.Conditions {
		if condition.Type == conditionType && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func approveKubeletCSRs(
	ctx context.Context,
	tenantClient client.Client,
	machine *clusterv1.Machine,
	csrs []*certificatesv1.CertificateSigningRequest,
	logger *slog.Logger,
	recorder events.EventRecorder,
) error {
	slices.SortFunc(csrs, func(left, right *certificatesv1.CertificateSigningRequest) int {
		return strings.Compare(left.Name, right.Name)
	})
	for _, csr := range csrs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := approveCSR(ctx, tenantClient, csr); err != nil {
			return err
		}
		if recorder != nil {
			recorder.Eventf(machine, nil, corev1.EventTypeNormal, KubeletCSRApprovedReason, approveCSRAction, "Approved kubelet CSR %s", csr.Name)
		}
		logger.Info("Approved kubelet CSR",
			"namespace", machine.Namespace,
			"clusterName", machine.Spec.ClusterName,
			"machineName", machine.Name,
			"csrName", csr.Name,
			"nodeName", expectedKubeletNodeName(machine),
		)
	}
	return nil
}

func approveCSR(ctx context.Context, tenantClient client.Client, csr *certificatesv1.CertificateSigningRequest) error {
	updated := csr.DeepCopy()
	now := metav1.Now()
	updated.Status.Conditions = append(updated.Status.Conditions, certificatesv1.CertificateSigningRequestCondition{
		Type:               certificatesv1.CertificateApproved,
		Status:             corev1.ConditionTrue,
		Reason:             KubeletClientCSRApprovalReason,
		Message:            "kubelet CSR matches the node identity recorded by NVKE",
		LastUpdateTime:     now,
		LastTransitionTime: now,
	})
	if err := tenantClient.SubResource("approval").Update(ctx, updated); err != nil {
		return fmt.Errorf("approving kubelet client CSR %s: %w", csr.Name, err)
	}
	return nil
}
