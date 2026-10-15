// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package tenantauth runs a short-lived credential pod that mints a real SPIRE
// JWT-SVID and exercises the tenant apiserver, then parses the result so the
// caller can assert identity mapping and RBAC.
package tenantauth

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/kube"
)

const (
	// podTimeout caps how long VerifySPIREAuth waits for the credential pod to finish.
	podTimeout = 5 * time.Minute
	resultYes  = "yes"
	resultNone = "none"
)

var (
	//go:embed reject.sh
	rejectScript string
	// ErrSPIREProbeInfrastructure means the pod did not prove that it minted and sent a token.
	ErrSPIREProbeInfrastructure = errors.New("SPIRE rejection probe infrastructure failure")
	// ErrSPIREAuthentication means the request reached the tenant API but had the wrong result.
	ErrSPIREAuthentication = errors.New("SPIRE authentication result mismatch")
)

// Identity describes the management-cluster workload used to exercise a tenant
// kubeconfig. CABundleConfigMap is optional and supplies trust for an external
// JWT exchange endpoint. JWTExchangeEndpoint supplies that endpoint to rejection
// probes, which cannot read it from their credential-free kubeconfig.
type Identity struct {
	Namespace, ServiceAccount string
	CABundleConfigMap         string
	JWTExchangeEndpoint       string
}

// Check is a single `kubectl auth can-i` query. Namespace: "" means
// cluster-scoped / no -n; "*" means --all-namespaces; otherwise -n <ns>.
type Check struct {
	Verb, Resource, Namespace string
}

// VerifyResult holds the parsed facts from a credential pod run.
type VerifyResult struct {
	Username    string         // from `auth whoami`; empty if auth was rejected
	CanI        map[Check]bool // per requested Check
	ListNodesOK bool           // whether `kubectl get nodes` (a real list call) succeeded
}

// VerifySPIREAuth runs one credential pod as id (namespace + service account)
// that waits for expectedUsername, mints a real JWT-SVID, and runs checks
// against the tenant clusterName. The pod writes its parseable output to the
// verify container's termination message, which is read back from the Pod
// object.
//
// A non-nil error indicates infra failure (pod Failed, couldn't read the
// result); an empty whoami or a "no" result is a reported fact for the caller
// to assert, not an error.
func VerifySPIREAuth(ctx context.Context, c client.Client, id Identity, clusterName, expectedUsername string, checks []Check) (VerifyResult, error) {
	pod := buildPod(id, clusterName, expectedUsername, checks)
	finished, verifyMsg, err := kube.RunPod(ctx, c, pod, verifyContainerName, podTimeout)
	if err != nil && finished != nil && finished.Status.Phase == corev1.PodFailed {
		return VerifyResult{}, fmt.Errorf(
			"verifier pod %s/%s failed (phase=%s); init %q message: %q; %q message: %q",
			id.Namespace, finished.Name, finished.Status.Phase,
			downloadContainerName, kube.TerminatedMessage(finished, downloadContainerName),
			verifyContainerName, verifyMsg,
		)
	}
	if err != nil {
		return VerifyResult{}, err
	}

	return parseOutput(verifyMsg), nil
}

// VerifySPIRERejected proves that the exec plugin minted a token and the tenant API returned HTTP 401.
func VerifySPIRERejected(ctx context.Context, c client.Client, id Identity, clusterName string) error {
	kubeconfig, err := spireRejectionKubeconfig(ctx, c, clusterName)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSPIREProbeInfrastructure, err)
	}
	env := []corev1.EnvVar{
		{Name: "SPIRE_KUBECONFIG", Value: string(kubeconfig)},
		{Name: "PLUGIN_INSTALL_PATH", Value: pluginInstallPath},
		{Name: "SPIFFE_JWT_EXCHANGE_ENDPOINT", Value: id.JWTExchangeEndpoint},
	}
	pod := buildCredentialPod(id, clusterName, rejectScript, env, nil)
	finished, message, err := kube.RunPod(ctx, c, pod, verifyContainerName, podTimeout)
	if err != nil {
		if finished != nil {
			return fmt.Errorf("%w: verifier pod %s/%s failed: %w", ErrSPIREProbeInfrastructure, finished.Namespace, finished.Name, err)
		}
		return fmt.Errorf("%w: verifier pod did not finish: %w", ErrSPIREProbeInfrastructure, err)
	}
	return classifyRejection(message)
}

func classifyRejection(message string) error {
	result, err := parseRejection(message)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSPIREProbeInfrastructure, err)
	}
	if !result.minted || (result.status != 200 && result.status != 401 && result.status != 403) || result.failure != resultNone {
		return fmt.Errorf("%w: minted=%t HTTP=%d command=%d failure=%s", ErrSPIREProbeInfrastructure, result.minted, result.status, result.commandExit, result.failure)
	}
	if result.status != 401 {
		return fmt.Errorf("%w: tenant API returned HTTP %d, want 401", ErrSPIREAuthentication, result.status)
	}
	if result.commandExit == 0 {
		return fmt.Errorf("%w: HTTP 401 request exited successfully", ErrSPIREProbeInfrastructure)
	}
	return nil
}

type rejectionResult struct {
	minted      bool
	status      int
	commandExit int
	failure     string
}

func parseRejection(message string) (rejectionResult, error) {
	var result rejectionResult
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(message), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || seen[fields[0]] {
			return rejectionResult{}, errors.New("verifier returned an invalid result")
		}
		seen[fields[0]] = true
		switch fields[0] {
		case "MINTED":
			if fields[1] != resultYes && fields[1] != "no" {
				return rejectionResult{}, errors.New("verifier returned an invalid mint result")
			}
			result.minted = fields[1] == resultYes
		case "HTTP":
			if fields[1] == resultNone {
				continue
			}
			status, err := strconv.Atoi(fields[1])
			if err != nil || status < 100 || status > 599 {
				return rejectionResult{}, errors.New("verifier returned an invalid HTTP status")
			}
			result.status = status
		case "COMMAND":
			exitCode, err := strconv.Atoi(fields[1])
			if err != nil || exitCode < 0 || exitCode > 255 {
				return rejectionResult{}, errors.New("verifier returned an invalid command status")
			}
			result.commandExit = exitCode
		case "FAILURE":
			switch fields[1] {
			case resultNone, "socket_missing", "plugin_missing", "mint_failed", "empty_token", "request_failed":
				result.failure = fields[1]
			default:
				return rejectionResult{}, errors.New("verifier returned an invalid failure detail")
			}
		default:
			return rejectionResult{}, errors.New("verifier returned an unknown field")
		}
	}
	if !seen["MINTED"] || !seen["HTTP"] || !seen["COMMAND"] || !seen["FAILURE"] {
		return rejectionResult{}, errors.New("verifier result is incomplete")
	}
	return result, nil
}
