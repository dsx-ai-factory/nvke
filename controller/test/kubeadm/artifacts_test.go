//go:build kubeadm_bringup

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubeadm

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"testing"
	"time"
)

// gatewayIP returns the .1 address of a /24, which setup-ci-host.sh gives the
// bridge and which every guest therefore sees as its default route.
func gatewayIP(cidr string) (string, error) {
	p, err := netip.ParsePrefix(cidr)
	if err != nil {
		return "", fmt.Errorf("parse bridge CIDR %q: %w", cidr, err)
	}
	addr := p.Masked().Addr().Next()
	if !addr.IsValid() {
		return "", fmt.Errorf("bridge CIDR %q has no usable gateway address", cidr)
	}
	return addr.String(), nil
}

// serveArtifacts publishes the two files community provisioning fetches and
// returns their guest-facing URLs.
//
// It binds the bridge gateway rather than every interface, so the files are
// reachable from the guests and from nowhere else, and it takes port 0 so
// repeated runs on a long-lived host cannot collide.
func serveArtifacts(t *testing.T, cfg config) (scriptURL, agentDebURL string) {
	t.Helper()

	for _, f := range []struct{ what, path string }{
		{"provision script", cfg.provisionScript},
		{"nke-agent .deb", cfg.agentDeb},
	} {
		if _, err := os.Stat(f.path); err != nil {
			t.Fatalf("%s not readable at %s: %v", f.what, f.path, err)
		}
	}

	gw, err := gatewayIP(cfg.bridgeCIDR)
	if err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", net.JoinHostPort(gw, "0"))
	if err != nil {
		t.Fatalf("serve artifacts on the bridge gateway %s: %v\n"+
			"the bridge must exist before the suite runs; scripts/setup-ci-host.sh creates it", gw, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/install-k8s.sh", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, cfg.provisionScript)
	})
	mux.HandleFunc("/nke-agent.deb", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, cfg.agentDeb)
	})

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Logf("artifact server stopped: %v", err)
		}
	}()
	t.Cleanup(func() { _ = srv.Close() })

	base := "http://" + ln.Addr().String()
	t.Logf("serving %s and %s on %s", cfg.provisionScript, cfg.agentDeb, base)
	return base + "/install-k8s.sh", base + "/nke-agent.deb"
}
