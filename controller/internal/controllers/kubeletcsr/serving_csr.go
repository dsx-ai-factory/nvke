// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubeletcsr

import (
	"context"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"net"
	"slices"

	certificatesv1 "k8s.io/api/certificates/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type trustedIdentities struct {
	dnsNames    map[string]struct{}
	ipAddresses []net.IP
}

func trustedServingIdentitiesForMachine(machine *clusterv1.Machine) trustedIdentities {
	trusted := trustedIdentities{dnsNames: map[string]struct{}{}}

	// The associated Kubernetes Node name is allowed as an exact DNS SAN.
	trusted.dnsNames[machine.Status.NodeRef.Name] = struct{}{}

	// CAPI Machine DNS addresses become trusted DNS SANs, IP addresses become trusted IP SANs,
	// and unknown address types are ignored.
	for _, address := range machine.Status.Addresses {
		switch address.Type {
		case clusterv1.MachineHostName, clusterv1.MachineInternalDNS, clusterv1.MachineExternalDNS:
			if isValidDNSName(address.Address) {
				trusted.dnsNames[address.Address] = struct{}{}
			}
		case clusterv1.MachineInternalIP, clusterv1.MachineExternalIP:
			if parsed := net.ParseIP(address.Address); parsed != nil {
				trusted.ipAddresses = append(trusted.ipAddresses, parsed)
			}
		default:
			// Ignore unknown address types.
		}
	}
	return trusted
}

func isValidDNSName(name string) bool {
	validationErrors := validation.IsDNS1123Subdomain(name)
	return len(validationErrors) == 0
}

type invalidServingCSR struct {
	csr    *certificatesv1.CertificateSigningRequest
	reason string
}

type servingCSRDecisions struct {
	pending []*certificatesv1.CertificateSigningRequest
	invalid []invalidServingCSR
}

func decideServingCSRs(csrs []certificatesv1.CertificateSigningRequest, nodeName string, trustedIdentities trustedIdentities) servingCSRDecisions {
	var csrDecisions servingCSRDecisions
	for i := range csrs {
		csr := &csrs[i]
		alreadyDecided := servingCSRDecided(csr)
		reason := validateServingCSR(csr, nodeName, trustedIdentities)

		switch {
		case alreadyDecided:
			// Already decided CSRs are ignored.
		case reason != "":
			csrDecisions.invalid = append(csrDecisions.invalid, invalidServingCSR{csr: csr, reason: reason})
		default:
			csrDecisions.pending = append(csrDecisions.pending, csr)
		}
	}
	return csrDecisions
}

func validateServingCSR(csr *certificatesv1.CertificateSigningRequest, nodeName string, trustedIdentities trustedIdentities) string {
	if csr.Spec.SignerName != certificatesv1.KubeletServingSignerName {
		return "spec.signerName is not " + certificatesv1.KubeletServingSignerName
	}

	expectedUsername := kubeletNodeUsername(nodeName)
	if csr.Spec.Username != expectedUsername {
		return "spec.username is not " + expectedUsername
	}

	if !slices.Contains(csr.Spec.Groups, kubeletNodeGroup) {
		return "spec.groups does not include system:nodes"
	}

	if !kubeletServingUsagesAllowed(csr.Spec.Usages) {
		return "spec.usages must be digital signature and server auth, with key encipherment optional"
	}

	parsed, reason := parseSingleCertificateRequest(csr.Spec.Request)
	if reason != "" {
		return reason
	}

	if err := parsed.CheckSignature(); err != nil {
		return "certificate request signature is invalid"
	}

	if reason := validateKubeletNodeSubject(parsed, nodeName); reason != "" {
		return reason
	}

	if reason := validateServingExtensions(parsed.Extensions, csr.Spec.Usages); reason != "" {
		return reason
	}

	if reason := validateServingSANs(parsed, trustedIdentities); reason != "" {
		return reason
	}

	return ""
}

// kubeletServingUsagesAllowed reports whether usages are allowed for the kubernetes.io/kubelet-serving signer.
// Digital signature and server auth are required. Key encipherment is optional, and all other usages are rejected.
func kubeletServingUsagesAllowed(usages []certificatesv1.KeyUsage) bool {
	hasSignature := false
	hasServerAuth := false
	for _, usage := range usages {
		switch usage { //nolint:exhaustive // every usage other than the three below is rejected
		case certificatesv1.UsageDigitalSignature:
			hasSignature = true
		case certificatesv1.UsageServerAuth:
			hasServerAuth = true
		case certificatesv1.UsageKeyEncipherment:
			// UsageKeyEncipherment is optionally allowed.
		default:
			return false
		}
	}

	// Assert the two required usages are present.
	return hasSignature && hasServerAuth
}

// validateServingExtensions allows key usage, extended key usage, and subjectAltName, and rejects every other extension.
// Key usage may set digital signature and key encipherment. Extended key usage must be server auth.
// A usage asserted by an extension must also be present in usages.
func validateServingExtensions(extensions []pkix.Extension, usages []certificatesv1.KeyUsage) string {
	return validateKubeletExtensions(extensions, usages, oidServerAuth, certificatesv1.UsageServerAuth, true)
}

func validateServingSANs(csr *x509.CertificateRequest, trustedIdentities trustedIdentities) string {
	if len(csr.EmailAddresses) > 0 || len(csr.URIs) > 0 {
		return "subject alternative names contain an unsupported identity"
	}

	if len(csr.DNSNames) == 0 && len(csr.IPAddresses) == 0 {
		return "certificate request has no subject alternative names"
	}

	for _, dnsName := range csr.DNSNames {
		if _, isTrustedDNSName := trustedIdentities.dnsNames[dnsName]; !isTrustedDNSName {
			return "DNS SAN is not trusted for the Machine"
		}
	}

	for _, ipAddress := range csr.IPAddresses {
		if !trustedIP(trustedIdentities.ipAddresses, ipAddress) {
			return "IP SAN is not trusted for the Machine"
		}
	}

	return ""
}

func trustedIP(trustedIPs []net.IP, ip net.IP) bool {
	return slices.ContainsFunc(trustedIPs, func(trustedIP net.IP) bool {
		return trustedIP.Equal(ip)
	})
}

func servingCSRsForMachine(ctx context.Context, tenantClient client.Client, nodeName string) ([]certificatesv1.CertificateSigningRequest, error) {
	list := &certificatesv1.CertificateSigningRequestList{}
	username := kubeletNodeUsername(nodeName)
	if err := tenantClient.List(ctx, list, client.MatchingFields{servingCSRUsernameField: username}); err != nil {
		return nil, fmt.Errorf("listing kubelet serving CSRs: %w", err)
	}
	return list.Items, nil
}

func servingCSRDecided(csr *certificatesv1.CertificateSigningRequest) bool {
	if len(csr.Status.Certificate) > 0 {
		return true
	}
	return csrConditionTrue(csr, certificatesv1.CertificateApproved) ||
		csrConditionTrue(csr, certificatesv1.CertificateDenied) ||
		csrConditionTrue(csr, certificatesv1.CertificateFailed)
}
