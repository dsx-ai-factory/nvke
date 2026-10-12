// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package site

// Deciding whether the controller is healthy, and waiting until it is.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/controller"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/kube"
)

const (
	// defaultReadyTimeout and defaultPollInterval bound ProbeController when a caller
	// states neither.
	defaultReadyTimeout = 10 * time.Minute
	defaultPollInterval = 10 * time.Second

	// defaultProbeTimeout bounds a single attempt, separately from the wait as
	// a whole. Sharing one deadline meant a request that connected and then
	// hung spent the entire ready timeout inside one attempt, so the ticker
	// never fired and the loop polled once. Sites failed that way against
	// /healthz while answering in under 4ms once reached by hand: the
	// port-forward reaches the pod as soon as it listens, which is before its
	// database and Temporal checks pass.
	defaultProbeTimeout = 30 * time.Second
)

// ProbeController polls until the controller answers /healthz through kubeconfig, then returns
// the version it reports.
//
// Only a run that deployed the controller has one to ask. A run that stopped at
// Kubernetes asserts against the API server instead, and expresses that by not
// calling this.
func ProbeController(ctx context.Context, kubeconfig string, within, poll time.Duration) (string, error) {
	return waitHealthy(ctx, within, poll, defaultProbeTimeout, func(ctx context.Context) (string, error) {
		return probeController(ctx, kubeconfig)
	})
}

// waitHealthy is ProbeController's retry, with the probe as a parameter so the timing
// can be tested without an API server. Unexported: the injection is this
// package's to use, not something a caller should have to supply.
//
// each bounds one attempt. It is separate from within, which bounds the wait
// as a whole, because an attempt that hangs must not consume the whole wait.
func waitHealthy(ctx context.Context, within, poll, each time.Duration, probe func(context.Context) (string, error)) (string, error) {
	if within <= 0 {
		within = defaultReadyTimeout
	}
	if poll <= 0 {
		poll = defaultPollInterval
	}
	if each <= 0 {
		each = defaultProbeTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, within)
	defer cancel()
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		version, err := probeOnce(ctx, each, probe)
		if err == nil {
			return version, nil
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("controller did not become healthy within %s: %w", within, err)
		case <-ticker.C:
		}
	}
}

// probeOnce bounds a single attempt, so a probe that connects and then hangs
// cannot swallow the whole wait.
func probeOnce(ctx context.Context, each time.Duration, probe func(context.Context) (string, error)) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, each)
	defer cancel()
	return probe(ctx)
}

var errUnhealthy = errors.New("controller unhealthy")

func probeController(ctx context.Context, kubeconfig string) (string, error) {
	restConfig, err := kube.RESTConfig(kubeconfig, "")
	if err != nil {
		return "", err
	}
	forwarder, err := kube.NewServiceForwarder(restConfig, controller.Namespace(), controller.Service(), controller.Port())
	if err != nil {
		return "", err
	}
	defer forwarder.Close()

	status, body, err := forwarder.Get(ctx, "/healthz")
	if err != nil {
		return "", err
	}
	if status != 200 {
		return "", fmt.Errorf("%w: /healthz returned %d: %s", errUnhealthy, status, body)
	}
	status, body, err = forwarder.Get(ctx, "/api/version")
	if err != nil {
		return "", err
	}
	if status != 200 {
		return "", fmt.Errorf("%w: /api/version returned %d: %s", errUnhealthy, status, body)
	}
	var v struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return "", fmt.Errorf("decode /api/version: %w", err)
	}
	return v.Version, nil
}
