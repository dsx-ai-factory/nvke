// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

// Package tenantauth runs a short-lived credential pod that mints a real SPIRE
// JWT-SVID and exercises the tenant apiserver, then parses the result so the
// caller can assert identity mapping and RBAC.
package tenantauth

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/NVIDIA/nke/controller/test/e2e/internal/kube"
)

const (
	// podTimeout caps how long VerifySPIREAuth waits for the credential pod to finish.
	podTimeout = 5 * time.Minute
)

// Identity is the (namespace, service account) a credential pod runs as; SPIRE
// attests it to spiffe://<td>/nke-service/<ServiceAccount>.
type Identity struct {
	Namespace, ServiceAccount string
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
// that mints a real JWT-SVID and runs checks against the tenant clusterName,
// then returns the parsed facts. The pod writes its parseable output to the
// verify container's termination message, which is read back from the Pod
// object.
//
// A non-nil error indicates infra failure (pod Failed, couldn't read the
// result); an empty whoami or a "no" result is a reported fact for the caller
// to assert, not an error.
func VerifySPIREAuth(ctx context.Context, c client.Client, id Identity, clusterName string, checks []Check) (VerifyResult, error) {
	pod := buildPod(id, clusterName, checks)
	finished, verifyMsg, err := kube.RunPod(ctx, c, pod, verifyContainerName, podTimeout)
	if err != nil && finished != nil && finished.Status.Phase == corev1.PodFailed {
		return VerifyResult{}, fmt.Errorf(
			"verifier pod %s/%s failed (phase=%s); init %q message: %q; %q message: %q",
			id.Namespace, finished.Name, finished.Status.Phase,
			downloadContainerName, kube.TerminatedMessage(finished, downloadContainerName),
			verifyContainerName, verifyMsg)
	}
	if err != nil {
		return VerifyResult{}, err
	}

	return parseOutput(verifyMsg), nil
}
