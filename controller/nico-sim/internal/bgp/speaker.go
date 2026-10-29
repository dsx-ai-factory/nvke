// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package bgp implements a lightweight BGP speaker that accepts kube-vip
// sessions and installs ECMP routes via netlink.
package bgp

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"runtime"
	"sort"
	"sync"
	"syscall"
	"time"

	bgpmsg "github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"github.com/vishvananda/netlink"
)

// rtnBlackhole is the netlink route type for a blackhole route (silently drops
// matching traffic). It equals golang.org/x/sys/unix.RTN_BLACKHOLE, inlined here
// because that constant is Linux-only and would break builds on non-Linux dev
// machines; netlink applies the value only on Linux anyway.
const rtnBlackhole = 6

// routeResyncInterval is how often announced VIP routes are re-applied.
const routeResyncInterval = 30 * time.Second

// Config holds BGP speaker configuration.
type Config struct {
	// Enabled controls whether the BGP speaker starts.
	Enabled bool

	// ListenAddr is the IP the BGP speaker binds to (bridge gateway). Empty
	// binds all interfaces.
	ListenAddr string

	// ListenPort is the TCP port for BGP. Default: 10179 (179 on darwin).
	ListenPort int

	// LocalASN is this speaker's AS number. Must match kube-vip's bgp_peeras.
	LocalASN uint32

	// PeerASN is the expected ASN of kube-vip peers.
	PeerASN uint32

	// HoldTime is the BGP hold timer in seconds. Default: 9.
	HoldTime uint16

	// KeepaliveInterval is the BGP keepalive interval in seconds. Default: 3.
	KeepaliveInterval uint16

	// BridgeSubnet is the CIDR of the bridge network.
	BridgeSubnet string

	// BridgeName is the network interface for route installation.
	BridgeName string

	// AllowedVIPCIDRs is an optional allowlist of CIDRs.
	AllowedVIPCIDRs []string
}

// RouteManager abstracts netlink operations for testability.
type RouteManager interface {
	RouteReplace(route *netlink.Route) error
	RouteDel(route *netlink.Route) error
	LinkByName(name string) (netlink.Link, error)
}

// netlinkRouteManager is the production RouteManager using real netlink calls.
type netlinkRouteManager struct{}

func (netlinkRouteManager) RouteReplace(route *netlink.Route) error {
	return netlink.RouteReplace(route)
}

func (netlinkRouteManager) RouteDel(route *netlink.Route) error {
	return netlink.RouteDel(route)
}

func (netlinkRouteManager) LinkByName(name string) (netlink.Link, error) {
	return netlink.LinkByName(name)
}

// Speaker is a lightweight BGP speaker that accepts kube-vip sessions
// and installs ECMP routes via netlink.
type Speaker struct {
	cfg         Config
	logger      *slog.Logger
	routes      RouteManager
	mu          sync.Mutex
	vipNexthops map[string]map[string]struct{} // VIP -> set of next-hop IPs
	bridgeNet   *net.IPNet
	allowedNets []*net.IPNet
	listener    net.Listener
}

// New creates a new BGP Speaker. Does not start it.
func New(cfg Config, logger *slog.Logger) *Speaker {
	if runtime.GOOS == "darwin" {
		return NewWithRouteManager(cfg, logger, routeCmdManager{run: sudoRoute, get: getRoute})
	}
	return NewWithRouteManager(cfg, logger, netlinkRouteManager{})
}

// NewWithRouteManager creates a Speaker with a custom RouteManager (for testing).
func NewWithRouteManager(cfg Config, logger *slog.Logger, rm RouteManager) *Speaker {
	s := &Speaker{
		cfg:         cfg,
		logger:      logger,
		routes:      rm,
		vipNexthops: make(map[string]map[string]struct{}),
	}

	// Parse bridge subnet for route validation.
	if _, ipNet, err := net.ParseCIDR(cfg.BridgeSubnet); err == nil {
		s.bridgeNet = ipNet
	}

	// Parse allowed VIP CIDRs.
	for _, cidr := range cfg.AllowedVIPCIDRs {
		if _, ipNet, err := net.ParseCIDR(cidr); err == nil {
			s.allowedNets = append(s.allowedNets, ipNet)
		} else {
			logger.Warn("Ignoring invalid VIP allowlist CIDR", "cidr", cidr, "error", err)
		}
	}

	return s
}

// Start initialises the TCP listener, accepts BGP peer connections,
// and monitors route changes. Blocks until ctx is cancelled.
func (s *Speaker) Start(ctx context.Context) error {
	addr := net.JoinHostPort(s.cfg.ListenAddr, fmt.Sprintf("%d", s.cfg.ListenPort))
	var lc net.ListenConfig
	var err error
	s.listener, err = lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("bgp: listen on %s: %w", addr, err)
	}

	s.logger.Info("BGP speaker started",
		"listenAddr", addr,
		"localASN", s.cfg.LocalASN,
		"peerASN", s.cfg.PeerASN,
		"holdTime", s.cfg.HoldTime,
		"keepalive", s.cfg.KeepaliveInterval,
	)

	// Install a covering blackhole route for each allowed VIP CIDR so VIP
	// traffic is dropped locally instead of leaking to the host default route
	// whenever no control-plane node is currently announcing a given VIP. The
	// per-VIP /32 ECMP routes installed on BGP announce are more specific and
	// win via longest-prefix match; when the last nexthop for a VIP is
	// withdrawn, its /32 is removed and this blackhole takes over again.
	// Not on darwin: the route manager there shells out through a sudoers rule
	// limited to host routes, so there is no covering route.
	if runtime.GOOS != "darwin" {
		s.ensureBlackholeRoutes()
	}

	var wg sync.WaitGroup
	// Close listener when context is cancelled.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctx.Done()
		s.listener.Close()
	}()
	// Routes are installed only when announcements change, but something else
	// on the host can delete them, as a VPN client on macOS does. Re-apply them
	// so a VIP does not stay unreachable until its control plane reconnects.
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(routeResyncInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.resyncRoutes()
			}
		}
	}()

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			s.logger.Warn("BGP accept error", "error", err)
			continue
		}

		peerAddr := conn.RemoteAddr().(*net.TCPAddr).IP.String()

		// Validate peer is from bridge subnet.
		if s.bridgeNet != nil {
			peerIP := net.ParseIP(peerAddr)
			if peerIP == nil || !s.bridgeNet.Contains(peerIP) {
				s.logger.Warn("Rejecting BGP peer outside bridge subnet",
					"peer", peerAddr, "bridgeSubnet", s.cfg.BridgeSubnet)
				conn.Close()
				continue
			}
		}

		s.logger.Info("BGP peer connected", "peer", peerAddr)
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.handlePeer(ctx, conn)
		}()
	}

	wg.Wait()
	return nil
}

// Stop logs active routes but does NOT delete them (VMs are still running).
func (s *Speaker) Stop() {
	if s.listener != nil {
		s.listener.Close()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for vip, hops := range s.vipNexthops {
		s.logger.Info("BGP speaker stopping, leaving route in place",
			"vip", vip, "nexthopCount", len(hops))
	}
}

// handlePeer runs the BGP FSM for a single peer connection.
//
// FSM: CONNECT → recv OPEN → validate ASN → send OPEN+KEEPALIVE → ESTABLISHED
// ESTABLISHED → recv UPDATE → processUpdate() → syncRoute()
// ESTABLISHED → recv KEEPALIVE → reset hold timer
// ESTABLISHED → hold timer expires → handlePeerDown() → close connection
// ESTABLISHED → connection error → handlePeerDown()
func (s *Speaker) handlePeer(ctx context.Context, conn net.Conn) {
	peerAddr := conn.RemoteAddr().(*net.TCPAddr).IP.String()
	defer func() {
		conn.Close()
		s.handlePeerDown(peerAddr)
		s.logger.Info("BGP peer disconnected", "peer", peerAddr)
	}()

	// Phase 1: Read OPEN from peer.
	msg, err := readBGPMessage(conn, 10*time.Second)
	if err != nil {
		s.logger.Warn("BGP failed to read OPEN", "peer", peerAddr, "error", err)
		return
	}

	openMsg, ok := msg.Body.(*bgpmsg.BGPOpen)
	if !ok {
		s.logger.Warn("BGP expected OPEN, got different message type", "peer", peerAddr)
		return
	}

	// Validate peer ASN. BGP4 uses MyAS (uint16), but for 4-byte ASN support,
	// the peer sets MyAS=AS_TRANS (23456) and sends the real ASN in a capability.
	peerASN := uint32(openMsg.MyAS)
	peerHasAS4 := false
	for _, param := range openMsg.OptParams {
		if paramCap, ok := param.(*bgpmsg.OptionParameterCapability); ok {
			for _, cap := range paramCap.Capability {
				if as4Cap, ok := cap.(*bgpmsg.CapFourOctetASNumber); ok {
					peerASN = as4Cap.CapValue
					peerHasAS4 = true
				}
			}
		}
	}
	if peerASN != s.cfg.PeerASN {
		s.logger.Warn("BGP peer ASN mismatch",
			"peer", peerAddr, "expected", s.cfg.PeerASN, "got", peerASN)
		return
	}

	// Phase 2: Send our OPEN + KEEPALIVE.
	// The local address, not ListenAddr: that is empty when binding all interfaces.
	routerID := conn.LocalAddr().(*net.TCPAddr).IP.String()

	// RFC 6793: a speaker with a 4-byte ASN MUST advertise AS_TRANS (23456) in the
	// 2-byte MyAS field and carry the real ASN in the 4-octet-ASN capability below.
	// Clamping to 65535 instead makes RFC-compliant peers (kube-vip/gobgp) reject our
	// OPEN as "Bad Peer AS", so the session never leaves ACTIVE and no VIP route is
	// advertised.
	myAS := uint16(s.cfg.LocalASN)
	if s.cfg.LocalASN > 65535 {
		myAS = bgpmsg.AS_TRANS
	}
	// gobgp v4 takes the router ID as a netip.Addr and returns an error, where
	// v3 took a string and could not fail.
	routerAddr, err := netip.ParseAddr(routerID)
	if err != nil {
		s.logger.Warn("BGP invalid router ID", "peer", peerAddr, "routerID", routerID, "error", err)
		return
	}
	ourOpen, err := bgpmsg.NewBGPOpenMessage(
		myAS, // MyAS (AS_TRANS for 4-byte ASNs; real ASN in the capability)
		s.cfg.HoldTime,
		routerAddr,
		[]bgpmsg.OptionParameterInterface{
			bgpmsg.NewOptionParameterCapability([]bgpmsg.ParameterCapabilityInterface{
				bgpmsg.NewCapFourOctetASNumber(s.cfg.LocalASN),
				bgpmsg.NewCapMultiProtocol(bgpmsg.RF_IPv4_UC),
			}),
		},
	)
	if err != nil {
		s.logger.Warn("BGP failed to build OPEN", "peer", peerAddr, "error", err)
		return
	}
	if err := writeBGPMessage(conn, ourOpen); err != nil {
		s.logger.Warn("BGP failed to send OPEN", "peer", peerAddr, "error", err)
		return
	}

	keepalive := bgpmsg.NewBGPKeepAliveMessage()
	if err := writeBGPMessage(conn, keepalive); err != nil {
		s.logger.Warn("BGP failed to send KEEPALIVE", "peer", peerAddr, "error", err)
		return
	}

	s.logger.Info("BGP session established", "peer", peerAddr, "peerASN", peerASN)

	// Phase 3: ESTABLISHED loop — process UPDATE and KEEPALIVE messages.
	holdDuration := time.Duration(s.cfg.HoldTime) * time.Second
	// gobgp v4 no longer detects the AS_PATH width from the bytes: it assumes
	// 4-byte ASNs unless told the peer did not negotiate them (RFC 6793).
	parseOpt := &bgpmsg.MarshallingOption{Use2ByteAS: !peerHasAS4}
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		msg, err := readBGPMessage(conn, holdDuration, parseOpt)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				s.logger.Warn("BGP read error", "peer", peerAddr, "error", err)
			}
			return
		}

		switch body := msg.Body.(type) {
		case *bgpmsg.BGPUpdate:
			s.processUpdate(peerAddr, body)
		case *bgpmsg.BGPKeepAlive:
			// Hold timer reset is implicit — readBGPMessage sets deadline.
		case *bgpmsg.BGPNotification:
			s.logger.Info("BGP received NOTIFICATION",
				"peer", peerAddr, "code", body.ErrorCode, "subcode", body.ErrorSubcode)
			return
		default:
			// Ignore unexpected message types.
		}

		// Send keepalive response.
		if err := writeBGPMessage(conn, keepalive); err != nil {
			s.logger.Warn("BGP failed to send KEEPALIVE", "peer", peerAddr, "error", err)
			return
		}
	}
}

// ipv4Prefix extracts the address and prefix length from an IPv4 unicast NLRI.
// gobgp v4 carries each route as a PathNLRI holding an NLRI interface, where v3
// exposed the prefix and length as fields. Any other address family is skipped:
// this speaker only advertises IPv4 unicast in its OPEN.
func ipv4Prefix(path bgpmsg.PathNLRI) (net.IP, int, bool) {
	p, ok := path.NLRI.(*bgpmsg.IPAddrPrefix)
	if !ok || !p.Prefix.IsValid() || !p.Prefix.Addr().Is4() {
		return nil, 0, false
	}
	return net.IP(p.Prefix.Addr().AsSlice()), p.Prefix.Bits(), true
}

// processUpdate extracts /32 prefixes and nexthops from a BGP UPDATE message.
// The nexthop is always the peer's TCP source address, not the BGP NEXT_HOP
// attribute. In the nico-vms topology these are always the same (kube-vip
// connects from the same IP it announces as nexthop), and using peerAddr
// ensures handlePeerDown can correctly clean up all routes for a disconnected
// peer without maintaining a separate peer→nexthop mapping.
func (s *Speaker) processUpdate(peerAddr string, update *bgpmsg.BGPUpdate) {
	nexthop := peerAddr

	// Process withdrawn routes.
	for _, withdrawn := range update.WithdrawnRoutes {
		addr, prefixLen, ok := ipv4Prefix(withdrawn)
		if !ok {
			continue
		}
		prefix := addr.String()
		if !s.isAllowedRoute(addr, prefixLen) {
			continue
		}
		s.mu.Lock()
		if hops, exists := s.vipNexthops[prefix]; exists {
			delete(hops, nexthop)
			if len(hops) == 0 {
				delete(s.vipNexthops, prefix)
			}
		}
		s.mu.Unlock()
		s.logger.Info("BGP route withdrawn", "peer", peerAddr, "vip", prefix, "nexthop", nexthop)
		s.syncRoute(prefix)
	}

	// Process NLRI (announced routes).
	for _, nlri := range update.NLRI {
		addr, prefixLen, ok := ipv4Prefix(nlri)
		if !ok {
			continue
		}
		prefix := addr.String()
		if !s.isAllowedRoute(addr, prefixLen) {
			s.logger.Error("BGP ignoring disallowed route",
				"peer", peerAddr, "prefix", prefix, "prefixLen", prefixLen)
			continue
		}
		s.mu.Lock()
		if s.vipNexthops[prefix] == nil {
			s.vipNexthops[prefix] = make(map[string]struct{})
		}
		s.vipNexthops[prefix][nexthop] = struct{}{}
		hopCount := len(s.vipNexthops[prefix])
		s.mu.Unlock()

		s.logger.Info("BGP route announced",
			"peer", peerAddr, "vip", prefix, "nexthop", nexthop, "nexthopCount", hopCount)
		if hopCount > 3 {
			s.logger.Warn("BGP VIP has more than 3 nexthops (possible duplicate VIP across clusters)",
				"vip", prefix, "nexthopCount", hopCount)
		}
		s.syncRoute(prefix)
	}
}

// handlePeerDown removes all nexthops for a disconnected peer and updates routes.
func (s *Speaker) handlePeerDown(peerAddr string) {
	s.mu.Lock()
	affectedVIPs := make([]string, 0)
	for vip, hops := range s.vipNexthops {
		if _, exists := hops[peerAddr]; exists {
			delete(hops, peerAddr)
			if len(hops) == 0 {
				delete(s.vipNexthops, vip)
			}
			affectedVIPs = append(affectedVIPs, vip)
		}
	}
	s.mu.Unlock()

	for _, vip := range affectedVIPs {
		s.logger.Info("BGP peer down, updating route", "peer", peerAddr, "vip", vip)
		s.syncRoute(vip)
	}
}

// resyncRoutes re-applies the route of every announced VIP.
func (s *Speaker) resyncRoutes() {
	s.mu.Lock()
	vips := make([]string, 0, len(s.vipNexthops))
	for vip := range s.vipNexthops {
		vips = append(vips, vip)
	}
	s.mu.Unlock()
	for _, vip := range vips {
		s.syncRoute(vip)
	}
}

// syncRoute computes current nexthops for a VIP and installs or removes the route.
func (s *Speaker) syncRoute(vip string) {
	s.mu.Lock()
	hops := s.vipNexthops[vip]
	nexthops := make([]net.IP, 0, len(hops))
	for nh := range hops {
		if ip := net.ParseIP(nh); ip != nil {
			nexthops = append(nexthops, ip)
		}
	}
	s.mu.Unlock()

	// Sort for deterministic route installation.
	sort.Slice(nexthops, func(i, j int) bool {
		return nexthops[i].String() < nexthops[j].String()
	})

	vipIP := net.ParseIP(vip)
	if vipIP == nil {
		s.logger.Error("BGP invalid VIP IP", "vip", vip)
		return
	}

	if len(nexthops) == 0 {
		if err := s.removeRoute(vipIP); err != nil {
			s.logger.Error("BGP failed to remove route", "vip", vip, "error", err)
		}
		return
	}

	if err := s.installMultipathRoute(vipIP, nexthops); err != nil {
		s.logger.Error("BGP failed to install ECMP route", "vip", vip, "error", err)
	}
}

// installMultipathRoute installs an ECMP multipath route for the VIP.
func (s *Speaker) installMultipathRoute(vip net.IP, nexthops []net.IP) error {
	link, err := s.routes.LinkByName(s.cfg.BridgeName)
	if err != nil {
		return fmt.Errorf("lookup bridge %s: %w", s.cfg.BridgeName, err)
	}
	route := &netlink.Route{
		Dst:       &net.IPNet{IP: vip.To4(), Mask: net.CIDRMask(32, 32)},
		MultiPath: make([]*netlink.NexthopInfo, len(nexthops)),
	}
	for i, nh := range nexthops {
		route.MultiPath[i] = &netlink.NexthopInfo{
			LinkIndex: link.Attrs().Index,
			Hops:      0, // equal weight
			Gw:        nh,
		}
	}
	return s.routes.RouteReplace(route)
}

// removeRoute deletes the route for a VIP when the last nexthop is withdrawn.
func (s *Speaker) removeRoute(vip net.IP) error {
	route := &netlink.Route{
		Dst: &net.IPNet{IP: vip.To4(), Mask: net.CIDRMask(32, 32)},
	}
	err := s.routes.RouteDel(route)
	if err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// ensureBlackholeRoutes installs a covering blackhole route for each allowed VIP
// CIDR. RouteReplace is idempotent, so this is safe to call on every start. The
// per-VIP /32 routes installed on BGP announce are more specific and take
// precedence via longest-prefix match; the blackhole only carries traffic for
// VIPs that no control-plane node is currently announcing, dropping it locally
// rather than leaking it to the host's default route.
func (s *Speaker) ensureBlackholeRoutes() {
	for _, n := range s.allowedNets {
		route := &netlink.Route{
			Dst:  n,
			Type: rtnBlackhole,
		}
		if err := s.routes.RouteReplace(route); err != nil {
			s.logger.Warn("BGP failed to install covering blackhole route for VIP CIDR",
				"cidr", n.String(), "error", err)
			continue
		}
		s.logger.Info("BGP installed covering blackhole route for VIP CIDR", "cidr", n.String())
	}
}

// isAllowedRoute validates whether a route prefix should be installed.
func (s *Speaker) isAllowedRoute(prefix net.IP, prefixLen int) bool {
	if prefixLen != 32 {
		return false
	}
	if prefix.IsLoopback() || prefix.IsLinkLocalUnicast() || prefix.IsMulticast() {
		return false
	}
	// Explicit allowlist takes priority over the bridge-subnet safety net.
	// This lets VIPs from a carved-out range within the bridge subnet work
	// when the operator (or auto-config) has deliberately allowed them.
	if len(s.allowedNets) > 0 {
		for _, n := range s.allowedNets {
			if n.Contains(prefix) {
				return true
			}
		}
		return false
	}
	// Without an allowlist, reject bridge-subnet IPs as a safety default.
	if s.bridgeNet != nil && s.bridgeNet.Contains(prefix) {
		return false
	}
	return true
}

// readBGPMessage reads a single BGP message from the connection with a deadline.
func readBGPMessage(conn net.Conn, timeout time.Duration, opts ...*bgpmsg.MarshallingOption) (*bgpmsg.BGPMessage, error) {
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return nil, fmt.Errorf("set read deadline: %w", err)
	}

	// Read the 19-byte BGP header: 16 marker + 2 length + 1 type.
	header := make([]byte, bgpmsg.BGP_HEADER_LENGTH)
	if _, err := io.ReadFull(conn, header); err != nil {
		return nil, err
	}

	// Parse length from header bytes 16-17 (big-endian).
	length := binary.BigEndian.Uint16(header[16:18])
	if length < bgpmsg.BGP_HEADER_LENGTH || length > bgpmsg.BGP_MAX_MESSAGE_LENGTH {
		return nil, fmt.Errorf("invalid BGP message length: %d", length)
	}

	// Read the remaining body bytes.
	body := make([]byte, length-bgpmsg.BGP_HEADER_LENGTH)
	if len(body) > 0 {
		if _, err := io.ReadFull(conn, body); err != nil {
			return nil, err
		}
	}

	// Combine header + body and parse. Written as an explicit allocation so
	// makezero passes; append(header, body...) is safe here because make()
	// returns cap == len, but the linter cannot see that.
	raw := make([]byte, 0, len(header)+len(body))
	raw = append(raw, header...)
	raw = append(raw, body...)
	msg, err := bgpmsg.ParseBGPMessage(raw, opts...)
	if err != nil {
		return nil, fmt.Errorf("parse BGP message: %w", err)
	}

	return msg, nil
}

// writeBGPMessage serializes and writes a BGP message to the connection.
func writeBGPMessage(conn net.Conn, msg *bgpmsg.BGPMessage) error {
	data, err := msg.Serialize()
	if err != nil {
		return fmt.Errorf("serialize BGP message: %w", err)
	}
	if err := conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return fmt.Errorf("set write deadline: %w", err)
	}
	_, err = conn.Write(data)
	return err
}
