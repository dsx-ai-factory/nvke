// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

// Reusable helpers for probing the local kube-apiserver's /readyz endpoint
// and waiting for it to come back after a mutation. Used by the etcd
// encryption flows (InitializeKubernetesAPIEncryption, key rotation) and by
// the apiserver auth-config flows (Enable/DisableAPIServerAuthenticationConfig),
// and intended to be the single landing zone for any future RPC that needs
// to gate on local apiserver readiness.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// apiServerReadyPollInterval is the wall-clock gap between consecutive
	// /readyz probes inside waitForAPIServerReady. Per-server overrides
	// live on NodeAgentServer.apiServerReadyPollInterval for tests.
	apiServerReadyPollInterval = 10 * time.Second

	// apiServerReadyMaxAttempts is the number of sleeps inside
	// waitForAPIServerReady; the total wall-clock budget is
	// apiServerReadyMaxAttempts * apiServerReadyPollInterval (~100s with
	// production constants). Per-server overrides live on
	// NodeAgentServer.apiServerReadyMaxAttempts for tests.
	apiServerReadyMaxAttempts = 10

	// apiserverReadyzProbeTimeout bounds a single GET /readyz call. Without
	// it a stalled TCP connect (apiserver listener up but kernel/queue blocked)
	// would hang the helper indefinitely and defeat the outer wait-loop cap.
	apiserverReadyzProbeTimeout = 3 * time.Second
)

// apiServerIsReady returns true iff the local kube-apiserver responds with
// HTTP 200 to GET https://127.0.0.1:6443/readyz. Any other outcome (non-2xx
// status, transport error, connection refused) returns false; the underlying
// error is logged but not returned, mirroring the polling semantics of the
// caller's wait loop.
//
// /readyz is the right signal (not /livez) because callers immediately issue
// authenticated client requests against the apiserver — for example, the
// post-mutation wait in InitializeKubernetesAPIEncryption is followed by
// PromoteKubernetesAPIEncryptionProviders fetching /metrics and writing the
// new encryption config. Those operations need informer caches synced, post-
// start hooks complete, and control loops running, none of which /livez gates.
//
// apiServerPodIsRunning is even weaker: pod.status.phase flips to "Running"
// within seconds of container start, while the apiserver itself takes
// ~15-30s longer to fully come up.
func (n *NodeAgentServer) apiServerIsReady() bool {
	ready, err := fetchApiserverReadyzFromRegistry(n.clientRegistry)
	if err != nil {
		slog.Error("kube-apiserver readiness probe error (will retry)", "error", err)
		return false
	}
	return ready
}

// waitForAPIServerReady polls the local apiserver's /readyz with the same
// 10x10s loop used by InitializeKubernetesAPIEncryption. Per-server
// overrides (NodeAgentServer.apiServerReadyMaxAttempts /
// .apiServerReadyPollInterval) take precedence over the package-level
// constants when non-zero.
//
// The loop probes (maxAttempts + 1) times with maxAttempts sleeps between
// probes; total wall-clock budget is maxAttempts * pollInterval (~100s with
// production constants).
func (s *NodeAgentServer) waitForAPIServerReady(ctx context.Context) error {
	maxAttempts := apiServerReadyMaxAttempts
	pollInterval := apiServerReadyPollInterval
	if s.apiServerReadyMaxAttempts > 0 {
		maxAttempts = s.apiServerReadyMaxAttempts
	}
	if s.apiServerReadyPollInterval > 0 {
		pollInterval = s.apiServerReadyPollInterval
	}

	for i := 0; i <= maxAttempts; i++ {
		if s.apiServerIsReady() {
			return nil
		}
		if i == maxAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
	return fmt.Errorf(
		"kube-apiserver did not become ready within %s",
		time.Duration(maxAttempts)*pollInterval,
	)
}

// fetchApiserverReadyzFromRegistry uses client-go to GET /readyz from the local
// apiserver at 127.0.0.1:6443. Returns true iff the response is HTTP 200,
// false on any non-2xx status, transport error, or connection refused.
//
// /readyz (rather than /livez) is the correct check for callers that will
// immediately issue real client requests against the apiserver: it gates on
// informer cache sync, post-start hooks, control loops, and absence of
// shutdown — none of which /livez covers.
func fetchApiserverReadyzFromRegistry(registry kubernetesClientRegistry) (bool, error) {
	if registry == nil {
		return false, errors.New("k8s client registry is not configured")
	}
	client, err := registry.LocalClient()
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), apiserverReadyzProbeTimeout)
	defer cancel()
	if err := client.RESTClient().Get().AbsPath("/readyz").Do(ctx).Error(); err != nil {
		return false, err
	}
	return true, nil
}

// readAPIServerMirrorPodUID returns the .metadata.uid of the kubelet mirror
// pod for the local kube-apiserver static pod (kube-apiserver-<hostname>
// in kube-system).
//
// Returns ("", nil) if the pod is NotFound — kubelet briefly destroys the OLD
// mirror pod before creating the NEW one during a static-pod restart, so a
// transient NotFound is a normal mid-swap state, not an error. Other errors
// (transport failure, hostname lookup failure, registry not configured) are
// returned to the caller.
//
// Used by waitForAPIServerReadyAfterMutation to anchor the static-pod swap
// against an atomic event boundary that /readyz alone cannot provide: the
// OLD apiserver pod keeps serving /readyz=200 throughout kubelet's
// --file-check-frequency window (default 20s) after a manifest mutation,
// so /readyz convergence is not by itself proof that the NEW pod is up.
//
// Goes through clientRegistry.Client() (the cluster-VIP-fronted client)
// rather than LocalClient() because the local apiserver may itself be mid-
// swap when this is called; the mirror pod is a regular K8s object that any
// apiserver in the control plane can return.
func (n *NodeAgentServer) readAPIServerMirrorPodUID(ctx context.Context) (string, error) {
	if n.clientRegistry == nil {
		return "", errors.New("k8s client registry is not configured")
	}
	hostname, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("hostname for mirror-pod lookup: %w", err)
	}
	client, err := n.clientRegistry.Client()
	if err != nil {
		return "", err
	}
	if client == nil {
		return "", errors.New("k8s client is nil (registry returned no client)")
	}
	podName := fmt.Sprintf("kube-apiserver-%s", hostname)
	pod, err := client.CoreV1().Pods("kube-system").Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", err
	}
	return string(pod.UID), nil
}

// waitForAPIServerReadyAfterMutation gates the existing /readyz poll loop
// on the kubelet mirror-pod UID flipping away from preMutationPodUID.
// Callers MUST read preMutationPodUID via readAPIServerMirrorPodUID(ctx)
// IMMEDIATELY before mutating /etc/kubernetes/manifests/kube-apiserver.yaml.
//
// preMutationPodUID == "" is permitted as a sentinel and falls back to
// waitForAPIServerReady semantics — there is no OLD pod to disambiguate
// from. Callers that fail to obtain a pre-mutation UID (e.g. apiserver was
// down before the mutation) should pass "" and accept the degraded gating.
//
// Without this gate, waitForAPIServerReady can return success against the
// OLD apiserver pod during kubelet's --file-check-frequency window
// (default 20s on stock kubeadm), where the OLD pod keeps serving
// /readyz=200 before kubelet detects the manifest change and starts the
// swap.
//
// Loop budget: total wall-clock is bounded by maxAttempts * pollInterval
// (~100s with production constants). Phase 1 (UID flip) and Phase 2
// (/readyz convergence) each have their own maxAttempts+1 probe count, so
// the worst-case wall-clock is ~2 * maxAttempts * pollInterval.
func (n *NodeAgentServer) waitForAPIServerReadyAfterMutation(
	ctx context.Context,
	preMutationPodUID string,
) error {
	if preMutationPodUID == "" {
		return n.waitForAPIServerReady(ctx)
	}

	maxAttempts := apiServerReadyMaxAttempts
	pollInterval := apiServerReadyPollInterval
	if n.apiServerReadyMaxAttempts > 0 {
		maxAttempts = n.apiServerReadyMaxAttempts
	}
	if n.apiServerReadyPollInterval > 0 {
		pollInterval = n.apiServerReadyPollInterval
	}

	// Phase 1: wait for the kubelet mirror-pod UID to flip.
	swapped := false
	for i := 0; i <= maxAttempts; i++ {
		cur, err := n.readAPIServerMirrorPodUID(ctx)
		switch {
		case err != nil:
			slog.Warn("apiserver mirror-pod read failed during swap-wait (will retry)",
				"preMutationPodUID", preMutationPodUID, "err", err)
		case cur == "" || cur != preMutationPodUID:
			// Either kubelet has destroyed the OLD mirror pod (NotFound,
			// returned as cur == "") or it has already created the NEW
			// pod with a different UID. Move on to Phase 2.
			swapped = true
		}
		if swapped {
			break
		}
		if i == maxAttempts {
			return fmt.Errorf(
				"kube-apiserver mirror pod UID did not change away from %s within %s",
				preMutationPodUID,
				time.Duration(maxAttempts)*pollInterval,
			)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}

	// Phase 2: existing /readyz convergence loop.
	return n.waitForAPIServerReady(ctx)
}
