// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bgp

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/vishvananda/netlink"
)

// routeCmdManager is the darwin RouteManager. macOS has no netlink and only
// root may change routes, so it shells out to `sudo -n /sbin/route -n`, which
// the sudoers rule in the nico-sim README allows for 172.31.0.x host routes.
// The argument order below must match that rule.
type routeCmdManager struct {
	run func(args ...string) ([]byte, error)
	get func(vip string) ([]byte, error)
}

// sudoRoute runs `sudo -n /sbin/route -n args...`, returning combined output.
func sudoRoute(args ...string) ([]byte, error) {
	return exec.Command("sudo", append([]string{"-n", "/sbin/route", "-n"}, args...)...).CombinedOutput()
}

// getRoute runs `route -n get vip`, which needs no privileges.
func getRoute(vip string) ([]byte, error) {
	return exec.Command("/sbin/route", "-n", "get", vip).CombinedOutput()
}

// CheckRouteSudo reports whether the sudoers rule for route installs is in
// place. `sudo -n -l` runs nothing and never prompts.
func CheckRouteSudo() error {
	args := []string{"-n", "-l", "/sbin/route", "-n", "add", "-host", "172.31.0.10", "192.168.64.2"}
	if out, err := exec.Command("sudo", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("passwordless sudo for /sbin/route host routes is not configured, "+
			"see the \"Full NVKE stack on macOS\" section of controller/nico-sim/README.md: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// RouteReplace installs a host route via the first next hop: macOS has no ECMP.
// The speaker sorts next hops, so the choice is stable and moves only when the
// announcing control plane changes.
//
// Whatever else the VIP resolves to is deleted and the route added afresh,
// rather than changed in place, because traffic to the VIP while it has no
// route leaves cloned entries that `route change` cannot fix:
//   - a host route through the default route (often a VPN), which `route
//     change` edits without making static, so it expires about twenty
//     minutes later and takes the VIP with it;
//   - an incomplete ARP entry scoped to the vmnet bridge, cloned from vmnet's
//     scoped default route, which shadows the static route until deleted.
func (m routeCmdManager) RouteReplace(route *netlink.Route) error {
	vip, gw := route.Dst.IP.String(), route.MultiPath[0].Gw.String()
	if current, err := m.get(vip); err == nil && isStaticRouteVia(string(current), gw) {
		return nil
	}
	gwRoute, err := m.get(gw)
	if err != nil {
		return fmt.Errorf("route get %s: %w: %s", gw, err, strings.TrimSpace(string(gwRoute)))
	}
	if err := m.deleteHost(vip); err != nil {
		return err
	}
	if err := m.deleteHost(vip, "-ifscope", routeInterface(string(gwRoute))); err != nil {
		return err
	}
	if out, err := m.run("add", "-host", vip, gw); err != nil {
		return fmt.Errorf("route %s via %s: %w: %s", vip, gw, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// isStaticRouteVia reports whether `route -n get` output shows a static route
// through gw.
func isStaticRouteVia(routeGet, gw string) bool {
	return strings.Contains(routeGet, "gateway: "+gw+"\n") && strings.Contains(routeGet, "STATIC")
}

// routeInterface returns the interface in `route -n get` output.
func routeInterface(routeGet string) string {
	for line := range strings.Lines(routeGet) {
		if ifc, ok := strings.CutPrefix(strings.TrimSpace(line), "interface: "); ok {
			return ifc
		}
	}
	return ""
}

// RouteDel removes a host route; a route that is already gone is success, like
// ESRCH from netlink.
func (m routeCmdManager) RouteDel(route *netlink.Route) error {
	return m.deleteHost(route.Dst.IP.String())
}

func (m routeCmdManager) deleteHost(vip string, scope ...string) error {
	out, err := m.run(append([]string{"delete", "-host", vip}, scope...)...)
	if err != nil && !strings.Contains(string(out), "not in table") {
		return fmt.Errorf("route delete %s %v: %w: %s", vip, scope, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// LinkByName returns a placeholder: routes are keyed by gateway, not interface.
func (routeCmdManager) LinkByName(string) (netlink.Link, error) {
	return &netlink.Dummy{}, nil
}
