// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bgp

import (
	"errors"
	"net"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
)

func TestRouteCmdManager(t *testing.T) {
	vip := &net.IPNet{IP: net.ParseIP("172.31.0.10").To4(), Mask: net.CIDRMask(32, 32)}
	multi := &netlink.Route{Dst: vip, MultiPath: []*netlink.NexthopInfo{{Gw: net.ParseIP("192.168.64.2")}, {Gw: net.ParseIP("192.168.64.3")}}}
	notInTable := []byte("route: writing to routing socket: not in table\n")
	// `route -n get` output, trimmed to the lines that matter.
	staticVia2 := "    gateway: 192.168.64.2\n  interface: bridge100\n      flags: <UP,GATEWAY,HOST,DONE,STATIC>\n"
	clonedVia2 := "    gateway: 192.168.64.2\n  interface: bridge100\n      flags: <UP,GATEWAY,HOST,DONE,WASCLONED,IFSCOPE>\n"
	staticVia3 := "    gateway: 192.168.64.3\n  interface: bridge100\n      flags: <UP,GATEWAY,HOST,DONE,STATIC>\n"
	viaDefault := "    gateway: 10.2.50.39\n  interface: utun4\n      flags: <UP,GATEWAY,DONE,STATIC,PRCLONING>\n"
	readd := [][]string{
		{"delete", "-host", "172.31.0.10"},
		{"delete", "-host", "172.31.0.10", "-ifscope", "bridge100"},
		{"add", "-host", "172.31.0.10", "192.168.64.2"},
	}

	tests := []struct {
		name    string
		call    func(routeCmdManager) error
		current string
		// errs are the errors of successive commands; output is shared by all.
		errs    []error
		output  []byte
		want    [][]string
		wantErr bool
	}{
		{
			name:    "replace leaves a static route via the first next hop alone",
			call:    func(m routeCmdManager) error { return m.RouteReplace(multi) },
			current: staticVia2,
		},
		{
			name:    "replace re-adds a cloned route as static",
			call:    func(m routeCmdManager) error { return m.RouteReplace(multi) },
			current: clonedVia2,
			want:    readd,
		},
		{
			name:    "replace moves a static route to the first next hop",
			call:    func(m routeCmdManager) error { return m.RouteReplace(multi) },
			current: staticVia3,
			want:    readd,
		},
		{
			name:    "replace adds when only the default route matches",
			call:    func(m routeCmdManager) error { return m.RouteReplace(multi) },
			current: viaDefault,
			errs:    []error{errors.New("exit 1")},
			output:  notInTable,
			want:    readd,
		},
		{
			name:    "replace surfaces other failures",
			call:    func(m routeCmdManager) error { return m.RouteReplace(multi) },
			current: viaDefault,
			errs:    []error{nil, nil, errors.New("exit 1")},
			output:  []byte("sudo: a password is required"),
			want:    readd,
			wantErr: true,
		},
		{
			name: "delete removes the host route",
			call: func(m routeCmdManager) error { return m.RouteDel(&netlink.Route{Dst: vip}) },
			want: [][]string{{"delete", "-host", "172.31.0.10"}},
		},
		{
			name:   "delete of an absent route is success",
			call:   func(m routeCmdManager) error { return m.RouteDel(&netlink.Route{Dst: vip}) },
			errs:   []error{errors.New("exit 1")},
			output: notInTable,
			want:   [][]string{{"delete", "-host", "172.31.0.10"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got [][]string
			m := routeCmdManager{
				run: func(args ...string) ([]byte, error) {
					var err error
					if len(got) < len(tt.errs) {
						err = tt.errs[len(got)]
					}
					got = append(got, args)
					return tt.output, err
				},
				get: func(dst string) ([]byte, error) {
					if dst == "192.168.64.2" {
						return []byte("  interface: bridge100\n"), nil
					}
					return []byte(tt.current), nil
				},
			}
			err := tt.call(m)
			require.Equal(t, tt.want, got)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
