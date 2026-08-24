// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"maps"
	"slices"
	"strings"
	"sync"

	"k8s.io/client-go/rest"
)

// Global, so the several places that build an NKE client share one tunnel per
// target rather than each opening their own.
//
//nolint:gochecknoglobals // deliberate: see above.
var (
	sharedForwardersMu sync.Mutex
	sharedForwarders   = map[string]*ServiceForwarder{}
)

// SharedServiceForwarder returns the process-wide forwarder for
// namespace/service:port, creating it on first use. Nothing is dialed here;
// call CloseSharedForwarders once at suite teardown.
func SharedServiceForwarder(restConfig *rest.Config, namespace, service, port string) (*ServiceForwarder, error) {
	key := strings.Join([]string{restConfig.Host, namespace, service, port}, "|")

	sharedForwardersMu.Lock()
	defer sharedForwardersMu.Unlock()

	if forwarder, ok := sharedForwarders[key]; ok {
		return forwarder, nil
	}

	forwarder, err := NewServiceForwarder(restConfig, namespace, service, port)
	if err != nil {
		return nil, err
	}
	sharedForwarders[key] = forwarder
	return forwarder, nil
}

// ResetSharedForwarders drops the live session of every shared forwarder and
// keeps the table, so a caller holding one from before the call still has a
// working value. Use this after replacing the pod the forwards point at.
func ResetSharedForwarders() {
	sharedForwardersMu.Lock()
	forwarders := slices.Collect(maps.Values(sharedForwarders))
	sharedForwardersMu.Unlock()

	for _, forwarder := range forwarders {
		forwarder.Reset()
	}
}

// CloseSharedForwarders tears down every forwarder SharedServiceForwarder handed
// out and empties the table, so the next caller gets a new one. Safe to call when
// none were created.
//
// A ServiceForwarder stays closed once closed, so a caller holding one from
// before this call has a dead value, not one that reconnects. Ask for the
// forwarder again after anything that calls this.
func CloseSharedForwarders() {
	sharedForwardersMu.Lock()
	forwarders := slices.Collect(maps.Values(sharedForwarders))
	clear(sharedForwarders)
	sharedForwardersMu.Unlock()

	for _, forwarder := range forwarders {
		forwarder.Close()
	}
}
