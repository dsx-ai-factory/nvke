// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package lifecycle

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dsx-ai-factory/nvke/controller/nico-sim/internal/state"
	vmruntime "github.com/dsx-ai-factory/nvke/controller/nico-sim/internal/vm"
)

// darwinNetwork uses Apple's DHCP allocator. It never writes the system lease
// database or dnsmasq reservations; boot data is delivered on the seed ISO.
type darwinNetwork struct {
	runtime                vmruntime.Runtime
	bridgeName, bridgeCIDR string
}

func (n darwinNetwork) EnsureSetup(ctx context.Context) error {
	return n.runtime.EnsureBridge(ctx, n.bridgeName, n.bridgeCIDR)
}

func (n darwinNetwork) NeedsDHCP(state.Instance) bool                                 { return true }
func (n darwinNetwork) WriteReservation(string, string, string, string, *slog.Logger) {}
func (n darwinNetwork) Teardown(context.Context) error                                { return nil }

// MetadataAddrs binds every interface: the vmnet gateway address exists only
// while some guest holds the shared network up, which nico-sim cannot rely on
// at startup. app.Setup restricts requests to the vmnet subnet.
func (n darwinNetwork) MetadataAddrs() []string { return []string{":7777"} }

// darwinUserData points phone_home at the vmnet gateway. CWE's user-data
// hardcodes 169.254.169.254, which only root can assign on macOS; left alone,
// every guest's cloud-init stalls ~20 minutes retrying it.
func darwinUserData(userData, gateway string) string {
	return strings.ReplaceAll(userData, "http://169.254.169.254:7777/", "http://"+net.JoinHostPort(gateway, "7777")+"/")
}

func (n darwinNetwork) LookupLease(_ context.Context, mac string) (string, bool, error) {
	data, err := os.ReadFile("/var/db/dhcpd_leases")
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read macOS DHCP leases: %w", err)
	}
	_, subnet, err := net.ParseCIDR(n.bridgeCIDR)
	if err != nil {
		return "", false, err
	}
	ip, ok := darwinLease(string(data), mac, subnet)
	return ip, ok, nil
}

// darwinLease returns the latest-expiring, non-expired lease for mac that
// falls within subnet. Filtering by subnet during selection (not after) is
// required: a newer lease on another vmnet subnet would otherwise win the
// "latest expiry" comparison and permanently mask a valid in-subnet lease.
func darwinLease(data, mac string, subnet *net.IPNet) (string, bool) {
	want, err := net.ParseMAC(mac)
	if err != nil {
		return "", false
	}
	var latest int64
	var found string
	for _, record := range strings.Split(data, "}") {
		var ip, hw string
		var expires int64
		for _, line := range strings.Split(record, "\n") {
			k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
			if !ok {
				continue
			}
			switch k {
			case "ip_address":
				ip = v
			case "lease":
				expires, _ = strconv.ParseInt(v, 0, 64)
			case "hw_address":
				_, hw, _ = strings.Cut(v, ",")
			}
		}
		// bootpd omits leading zeroes in MAC octets.
		parts := strings.Split(hw, ":")
		for i, p := range parts {
			if len(p) == 1 {
				parts[i] = "0" + p
			}
		}
		parsed, err := net.ParseMAC(strings.Join(parts, ":"))
		parsedIP := net.ParseIP(ip)
		if err == nil && parsed.String() == want.String() && parsedIP != nil &&
			(subnet == nil || subnet.Contains(parsedIP)) &&
			expires > time.Now().Unix() && expires > latest {
			found, latest = ip, expires
		}
	}
	return found, found != ""
}
