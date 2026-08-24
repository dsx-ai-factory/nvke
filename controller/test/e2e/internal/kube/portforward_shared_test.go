// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"testing"

	"k8s.io/client-go/rest"
)

// Nothing is dialed here: SharedServiceForwarder only builds the clientset and
// records the forwarder, so an unreachable host is enough.
func sharedTestConfig() *rest.Config {
	return &rest.Config{Host: "https://apiserver.invalid"}
}

// The rollout path. ResetSharedForwarders keeps the table and the objects in it,
// so a caller holding a forwarder from before the reset still has a working one;
// only the live session is dropped, and the next request rebuilds it.
//
// nke.setNKEContainerEnv uses this rather than Close for exactly that reason:
// the API client in internal/nke captures a forwarder when it is built, which is
// before any rollout the specs perform.
func TestResetSharedForwardersKeepsTheForwarderUsable(t *testing.T) {
	CloseSharedForwarders()
	t.Cleanup(CloseSharedForwarders)

	held, err := SharedServiceForwarder(sharedTestConfig(), "ns", "svc", "8080")
	if err != nil {
		t.Fatalf("SharedServiceForwarder: %v", err)
	}

	ResetSharedForwarders()

	if held.closed {
		t.Fatal("Reset must not close the forwarder; a closed one never reconnects")
	}
	if held.current != nil {
		t.Fatal("Reset must drop the live session so the next request rebuilds it")
	}

	after, err := SharedServiceForwarder(sharedTestConfig(), "ns", "svc", "8080")
	if err != nil {
		t.Fatalf("SharedServiceForwarder after reset: %v", err)
	}
	if after != held {
		t.Fatal("Reset must keep the table, so a held forwarder stays the shared one")
	}
}

// One forwarder per target, and a new one after Close.
//
// nke.SetOIDCEnabled and nke.SetPermissiveMode rely on the second half: they drop
// the shared forwards once a rollout completes, so the next request builds a
// tunnel to the replacement pod instead of reusing one aimed at a pod that is
// gone. A Close that left the table populated would hand back the closed
// forwarder, which never reconnects.
func TestSharedServiceForwarderReplacesAfterClose(t *testing.T) {
	CloseSharedForwarders()
	t.Cleanup(CloseSharedForwarders)

	first, err := SharedServiceForwarder(sharedTestConfig(), "ns", "svc", "8080")
	if err != nil {
		t.Fatalf("first SharedServiceForwarder: %v", err)
	}

	same, err := SharedServiceForwarder(sharedTestConfig(), "ns", "svc", "8080")
	if err != nil {
		t.Fatalf("second SharedServiceForwarder: %v", err)
	}
	if same != first {
		t.Fatal("one target should share one forwarder")
	}

	other, err := SharedServiceForwarder(sharedTestConfig(), "ns", "svc", "metrics")
	if err != nil {
		t.Fatalf("SharedServiceForwarder for a second port: %v", err)
	}
	if other == first {
		t.Fatal("a different port is a different target and needs its own forwarder")
	}

	CloseSharedForwarders()

	replacement, err := SharedServiceForwarder(sharedTestConfig(), "ns", "svc", "8080")
	if err != nil {
		t.Fatalf("SharedServiceForwarder after close: %v", err)
	}
	if replacement == first {
		t.Fatal("after CloseSharedForwarders the table must hand out a new forwarder, not the closed one")
	}
}
