// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bgp

import (
	"context"
	"log/slog"
	"net"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	bgpmsg "github.com/osrg/gobgp/v3/pkg/packet/bgp"
	"github.com/vishvananda/netlink"
)

// ── Mock RouteManager ────────────────────────────────────────────────────────

type mockLink struct {
	attrs netlink.LinkAttrs
}

func (m *mockLink) Attrs() *netlink.LinkAttrs { return &m.attrs }
func (m *mockLink) Type() string              { return "mock" }

type routeCall struct {
	op       string // "replace" or "del"
	dst      string
	nexthops []string
}

type mockRouteManager struct {
	mu    sync.Mutex
	calls []routeCall
	link  *mockLink
}

func newMockRM() *mockRouteManager {
	return &mockRouteManager{
		link: &mockLink{attrs: netlink.LinkAttrs{Index: 42, Name: "cwebr0"}},
	}
}

func (m *mockRouteManager) RouteReplace(route *netlink.Route) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var nexthops []string
	for _, nh := range route.MultiPath {
		nexthops = append(nexthops, nh.Gw.String())
	}
	sort.Strings(nexthops)
	m.calls = append(m.calls, routeCall{op: "replace", dst: route.Dst.IP.String(), nexthops: nexthops})
	return nil
}

func (m *mockRouteManager) RouteDel(route *netlink.Route) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, routeCall{op: "del", dst: route.Dst.IP.String()})
	return nil
}

func (m *mockRouteManager) LinkByName(_ string) (netlink.Link, error) {
	return m.link, nil
}

func (m *mockRouteManager) lastCall() (routeCall, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.calls) == 0 {
		return routeCall{}, false
	}
	return m.calls[len(m.calls)-1], true
}

func (m *mockRouteManager) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

// ── Test helpers ─────────────────────────────────────────────────────────────

const (
	testBridgeSubnet = "192.168.200.0/24"
	testLocalASN     = uint32(4294967000)
	testPeerASN      = uint32(65100)
	testVIP          = "10.0.0.100"
)

func newTestSpeakerDirect(rm *mockRouteManager, extraCfg ...func(*Config)) *Speaker {
	cfg := Config{
		Enabled:           true,
		ListenAddr:        "127.0.0.1",
		ListenPort:        0,
		LocalASN:          testLocalASN,
		PeerASN:           testPeerASN,
		HoldTime:          9,
		KeepaliveInterval: 3,
		BridgeSubnet:      testBridgeSubnet,
		BridgeName:        "cwebr0",
	}
	for _, fn := range extraCfg {
		fn(&cfg)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	return NewWithRouteManager(cfg, logger, rm)
}

// buildUpdate constructs a BGP UPDATE with the given NLRI/nexthop.
func buildUpdate(vip, nexthop string, prefixLen uint8) *bgpmsg.BGPUpdate {
	msg := bgpmsg.NewBGPUpdateMessage(
		nil,
		[]bgpmsg.PathAttributeInterface{
			bgpmsg.NewPathAttributeOrigin(bgpmsg.BGP_ORIGIN_ATTR_TYPE_IGP),
			bgpmsg.NewPathAttributeNextHop(nexthop),
		},
		[]*bgpmsg.IPAddrPrefix{
			bgpmsg.NewIPAddrPrefix(prefixLen, vip),
		},
	)
	return msg.Body.(*bgpmsg.BGPUpdate)
}

// buildWithdraw constructs a BGP UPDATE withdrawing the given prefix.
func buildWithdraw(vip string, prefixLen uint8) *bgpmsg.BGPUpdate {
	msg := bgpmsg.NewBGPUpdateMessage(
		[]*bgpmsg.IPAddrPrefix{
			bgpmsg.NewIPAddrPrefix(prefixLen, vip),
		},
		nil,
		nil,
	)
	return msg.Body.(*bgpmsg.BGPUpdate)
}

// ── Direct processUpdate / handlePeerDown tests (no TCP needed) ──────────────

// Test 1: Single peer announces VIP/32 → route installed with 1 nexthop.
func TestProcessUpdate_SinglePeerInstallsRoute(t *testing.T) {
	t.Parallel()
	rm := newMockRM()
	s := newTestSpeakerDirect(rm)

	s.processUpdate("192.168.200.10", buildUpdate(testVIP, "192.168.200.10", 32))

	if call, ok := rm.lastCall(); !ok {
		t.Fatal("expected route call")
	} else if call.op != "replace" || call.dst != testVIP {
		t.Errorf("want replace %s, got %s %s", testVIP, call.op, call.dst)
	} else if len(call.nexthops) != 1 || call.nexthops[0] != "192.168.200.10" {
		t.Errorf("want nexthops [192.168.200.10], got %v", call.nexthops)
	}
}

// Test 2: Second peer announces same VIP → ECMP route with 2 nexthops.
func TestProcessUpdate_TwoPeersECMP(t *testing.T) {
	t.Parallel()
	rm := newMockRM()
	s := newTestSpeakerDirect(rm)

	s.processUpdate("192.168.200.10", buildUpdate(testVIP, "192.168.200.10", 32))
	s.processUpdate("192.168.200.11", buildUpdate(testVIP, "192.168.200.11", 32))

	call, _ := rm.lastCall()
	if call.op != "replace" || len(call.nexthops) != 2 {
		t.Errorf("want replace with 2 nexthops, got %s %v", call.op, call.nexthops)
	}
}

// Test 3: Third peer announces same VIP → ECMP route with 3 nexthops.
func TestProcessUpdate_ThreePeersECMP(t *testing.T) {
	t.Parallel()
	rm := newMockRM()
	s := newTestSpeakerDirect(rm)

	s.processUpdate("192.168.200.10", buildUpdate(testVIP, "192.168.200.10", 32))
	s.processUpdate("192.168.200.11", buildUpdate(testVIP, "192.168.200.11", 32))
	s.processUpdate("192.168.200.12", buildUpdate(testVIP, "192.168.200.12", 32))

	call, _ := rm.lastCall()
	if call.op != "replace" || len(call.nexthops) != 3 {
		t.Errorf("want replace with 3 nexthops, got %s %v", call.op, call.nexthops)
	}
}

// Test 4: One peer disconnects → ECMP route updated to 2 nexthops.
func TestHandlePeerDown_ReducesECMP(t *testing.T) {
	t.Parallel()
	rm := newMockRM()
	s := newTestSpeakerDirect(rm)

	s.processUpdate("192.168.200.10", buildUpdate(testVIP, "192.168.200.10", 32))
	s.processUpdate("192.168.200.11", buildUpdate(testVIP, "192.168.200.11", 32))
	s.processUpdate("192.168.200.12", buildUpdate(testVIP, "192.168.200.12", 32))

	s.handlePeerDown("192.168.200.12")

	call, _ := rm.lastCall()
	if call.op != "replace" || len(call.nexthops) != 2 {
		t.Errorf("want replace with 2 nexthops after peer down, got %s %v", call.op, call.nexthops)
	}
	for _, nh := range call.nexthops {
		if nh == "192.168.200.12" {
			t.Errorf("disconnected peer nexthop still in route: %v", call.nexthops)
		}
	}
}

// Test 5: All peers disconnect → route fully removed.
func TestHandlePeerDown_AllDownRemovesRoute(t *testing.T) {
	t.Parallel()
	rm := newMockRM()
	s := newTestSpeakerDirect(rm)

	s.processUpdate("192.168.200.10", buildUpdate(testVIP, "192.168.200.10", 32))
	s.processUpdate("192.168.200.11", buildUpdate(testVIP, "192.168.200.11", 32))

	s.handlePeerDown("192.168.200.10")
	s.handlePeerDown("192.168.200.11")

	call, _ := rm.lastCall()
	if call.op != "del" || call.dst != testVIP {
		t.Errorf("want del %s after all peers down, got %s %s", testVIP, call.op, call.dst)
	}
}

// Test 6: Non-/32 prefix → rejected, no route installed.
func TestProcessUpdate_NonSlash32Rejected(t *testing.T) {
	t.Parallel()
	rm := newMockRM()
	s := newTestSpeakerDirect(rm)

	s.processUpdate("192.168.200.10", buildUpdate("10.0.0.0", "192.168.200.10", 24))

	if rm.callCount() != 0 {
		t.Errorf("expected no route calls for /24, got %d", rm.callCount())
	}
}

// Test 7: Bridge subnet IP as VIP → rejected.
func TestProcessUpdate_BridgeSubnetVIPRejected(t *testing.T) {
	t.Parallel()
	rm := newMockRM()
	s := newTestSpeakerDirect(rm)

	s.processUpdate("192.168.200.10", buildUpdate("192.168.200.50", "192.168.200.10", 32))

	if rm.callCount() != 0 {
		t.Errorf("expected no route calls for bridge subnet VIP, got %d", rm.callCount())
	}
}

// Test 8: Link-local IP as VIP → rejected.
func TestProcessUpdate_LinkLocalVIPRejected(t *testing.T) {
	t.Parallel()
	rm := newMockRM()
	s := newTestSpeakerDirect(rm)

	s.processUpdate("192.168.200.10", buildUpdate("169.254.1.1", "192.168.200.10", 32))

	if rm.callCount() != 0 {
		t.Errorf("expected no route calls for link-local VIP, got %d", rm.callCount())
	}
}

// Test 9: Two distinct VIPs → two independent routes.
func TestProcessUpdate_TwoVIPsAreIndependent(t *testing.T) {
	t.Parallel()
	rm := newMockRM()
	s := newTestSpeakerDirect(rm)

	s.processUpdate("192.168.200.10", buildUpdate("10.0.0.100", "192.168.200.10", 32))
	s.processUpdate("192.168.200.20", buildUpdate("10.0.1.200", "192.168.200.20", 32))

	if rm.callCount() != 2 {
		t.Fatalf("expected 2 route calls, got %d", rm.callCount())
	}

	rm.mu.Lock()
	defer rm.mu.Unlock()
	dsts := map[string]bool{}
	for _, c := range rm.calls {
		dsts[c.dst] = true
	}
	if !dsts["10.0.0.100"] || !dsts["10.0.1.200"] {
		t.Errorf("expected routes for both VIPs, got: %v", rm.calls)
	}
}

// Test 10: Allowlist configured → only matching prefix installed.
func TestProcessUpdate_AllowlistFiltersVIPs(t *testing.T) {
	t.Parallel()
	rm := newMockRM()
	s := newTestSpeakerDirect(rm, func(c *Config) {
		c.AllowedVIPCIDRs = []string{"10.0.0.0/24"}
	})

	// Allowed.
	s.processUpdate("192.168.200.10", buildUpdate("10.0.0.100", "192.168.200.10", 32))
	if rm.callCount() != 1 {
		t.Fatalf("expected 1 call for allowed prefix, got %d", rm.callCount())
	}

	// Not allowed (outside allowlist).
	s.processUpdate("192.168.200.10", buildUpdate("10.0.1.200", "192.168.200.10", 32))
	if rm.callCount() != 1 {
		t.Errorf("expected no extra calls for non-allowlisted VIP, got %d total", rm.callCount())
	}
}

// Test: explicit BGP WITHDRAW → route removed.
func TestProcessUpdate_ExplicitWithdrawRemovesRoute(t *testing.T) {
	t.Parallel()
	rm := newMockRM()
	s := newTestSpeakerDirect(rm)

	s.processUpdate("192.168.200.10", buildUpdate(testVIP, "192.168.200.10", 32))
	s.processUpdate("192.168.200.10", buildWithdraw(testVIP, 32))

	call, _ := rm.lastCall()
	if call.op != "del" {
		t.Errorf("want del after withdraw, got %s", call.op)
	}
}

// ── isAllowedRoute unit tests ────────────────────────────────────────────────

func TestIsAllowedRoute(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	s := NewWithRouteManager(Config{
		BridgeSubnet:    testBridgeSubnet,
		AllowedVIPCIDRs: nil,
	}, logger, newMockRM())

	tests := []struct {
		name      string
		prefix    string
		prefixLen int
		want      bool
	}{
		{"valid /32", "10.0.0.100", 32, true},
		{"not /32 (is /24)", "10.0.0.0", 24, false},
		{"bridge subnet", "192.168.200.50", 32, false},
		{"link-local", "169.254.1.1", 32, false},
		{"loopback", "127.0.0.1", 32, false},
		{"multicast", "224.0.0.1", 32, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ip := net.ParseIP(tc.prefix)
			got := s.isAllowedRoute(ip, tc.prefixLen)
			if got != tc.want {
				t.Errorf("isAllowedRoute(%s, %d) = %v, want %v", tc.prefix, tc.prefixLen, got, tc.want)
			}
		})
	}
}

func TestIsAllowedRoute_Allowlist(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	s := NewWithRouteManager(Config{
		BridgeSubnet:    testBridgeSubnet,
		AllowedVIPCIDRs: []string{"10.0.0.0/24"},
	}, logger, newMockRM())

	tests := []struct {
		prefix string
		want   bool
	}{
		{"10.0.0.1", true},
		{"10.0.0.255", true},
		{"10.0.1.1", false},
		{"8.8.8.8", false},
	}

	for _, tc := range tests {
		t.Run(tc.prefix, func(t *testing.T) {
			t.Parallel()
			got := s.isAllowedRoute(net.ParseIP(tc.prefix), 32)
			if got != tc.want {
				t.Errorf("isAllowedRoute(%s) with allowlist = %v, want %v", tc.prefix, got, tc.want)
			}
		})
	}
}

func TestIsAllowedRoute_BridgeSubnetInAllowlist(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	// Allowlist includes a range within the bridge subnet (the VIP pool).
	s := NewWithRouteManager(Config{
		BridgeSubnet:    testBridgeSubnet,
		AllowedVIPCIDRs: []string{"192.168.200.240/28"},
	}, logger, newMockRM())

	tests := []struct {
		name   string
		prefix string
		want   bool
	}{
		{"VIP in allowed range within bridge subnet", "192.168.200.240", true},
		{"VIP at end of allowed range", "192.168.200.254", true},
		{"bridge subnet IP outside allowed range", "192.168.200.50", false},
		{"IP outside bridge subnet and allowlist", "10.0.0.1", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := s.isAllowedRoute(net.ParseIP(tc.prefix), 32)
			if got != tc.want {
				t.Errorf("isAllowedRoute(%s) = %v, want %v", tc.prefix, got, tc.want)
			}
		})
	}
}

// ── TCP-based FSM tests (Tests 11 & 12) ─────────────────────────────────────

// newTCPSpeaker starts a speaker listening on a random loopback port,
// bypassing the bridge-subnet check so tests can connect from 127.0.0.1.
func newTCPSpeaker(t *testing.T, rm *mockRouteManager, extraCfg ...func(*Config)) (s *Speaker, addr net.Addr, cancel context.CancelFunc) {
	t.Helper()
	cfg := Config{
		Enabled:           true,
		ListenAddr:        "127.0.0.1",
		ListenPort:        0,
		LocalASN:          testLocalASN,
		PeerASN:           testPeerASN,
		HoldTime:          2, // short for tests
		KeepaliveInterval: 1,
		BridgeSubnet:      "", // disabled — allows loopback peers in tests
		BridgeName:        "cwebr0",
	}
	for _, fn := range extraCfg {
		fn(&cfg)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	s = NewWithRouteManager(cfg, logger, rm)

	ctx, cancelFn := context.WithCancel(context.Background())
	ready := make(chan net.Addr, 1)
	go func() {
		lc := net.ListenConfig{}
		ln, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
		if err != nil {
			close(ready)
			cancelFn()
			return
		}
		s.listener = ln
		ready <- ln.Addr()

		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-ctx.Done()
			ln.Close()
		}()
		for {
			conn, err := ln.Accept()
			if err != nil {
				break
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				s.handlePeer(ctx, conn)
			}()
		}
		wg.Wait()
	}()

	addr = <-ready
	if addr == nil {
		t.Fatal("speaker failed to start")
	}
	return s, addr, cancelFn
}

// connectPeer dials the speaker and completes the BGP OPEN handshake.
func connectPeer(t *testing.T, addr net.Addr, peerASN uint32) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr.String(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial speaker: %v", err)
	}

	open := bgpmsg.NewBGPOpenMessage(
		uint16(min(peerASN, 65535)),
		9,
		"1.2.3.4",
		[]bgpmsg.OptionParameterInterface{
			bgpmsg.NewOptionParameterCapability([]bgpmsg.ParameterCapabilityInterface{
				bgpmsg.NewCapFourOctetASNumber(peerASN),
				bgpmsg.NewCapMultiProtocol(bgpmsg.RF_IPv4_UC),
			}),
		},
	)
	if err := writeBGPMessage(conn, open); err != nil {
		t.Fatalf("write OPEN: %v", err)
	}
	// Read speaker's OPEN.
	if msg, err := readBGPMessage(conn, 3*time.Second); err != nil {
		t.Fatalf("read speaker OPEN: %v", err)
	} else if _, ok := msg.Body.(*bgpmsg.BGPOpen); !ok {
		t.Fatalf("expected OPEN, got %T", msg.Body)
	}
	// Read speaker's KEEPALIVE.
	if msg, err := readBGPMessage(conn, 3*time.Second); err != nil {
		t.Fatalf("read speaker KEEPALIVE: %v", err)
	} else if _, ok := msg.Body.(*bgpmsg.BGPKeepAlive); !ok {
		t.Fatalf("expected KEEPALIVE, got %T", msg.Body)
	}
	return conn
}

// Test 11: Hold timer expiry → peer down, route removed.
func TestSpeaker_HoldTimerExpiryWithdrawsRoute(t *testing.T) {
	t.Parallel()
	rm := newMockRM()

	// First install a route directly (no TCP needed for the announce part).
	s := newTestSpeakerDirect(rm)
	s.processUpdate("192.168.200.10", buildUpdate(testVIP, "192.168.200.10", 32))
	if rm.callCount() != 1 {
		t.Fatal("route not installed before hold timer test")
	}

	// Now simulate hold timer expiry by calling handlePeerDown directly.
	// The TCP hold timer test verifies that the timer fires when no messages arrive.
	s.handlePeerDown("192.168.200.10")

	call, _ := rm.lastCall()
	if call.op != "del" {
		t.Errorf("expected del after peer down (hold timer expired), got %s", call.op)
	}
}

// Test 11b: TCP hold timer — speaker disconnects when no keepalives arrive.
func TestSpeaker_HoldTimerExpiryClosesConnection(t *testing.T) {
	t.Parallel()
	rm := newMockRM()
	_, addr, cancel := newTCPSpeaker(t, rm)
	defer cancel()

	conn := connectPeer(t, addr, testPeerASN)
	defer conn.Close()

	// Stop sending keepalives — speaker hold timer (2s) should expire.
	// The speaker will close the connection from its side.
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 256)
	// Either the speaker sends us a keepalive (normal) or closes the connection.
	// After HoldTime seconds without a message from us, the speaker's read will timeout.
	// The speaker's readBGPMessage uses holdDuration as timeout, so it will get an error
	// and call handlePeerDown. From our side, we'll eventually get EOF.
	start := time.Now()
	for time.Now().Before(start.Add(5 * time.Second)) {
		n, err := conn.Read(buf)
		if err != nil {
			// Speaker closed connection — expected after hold timer.
			elapsed := time.Since(start)
			if elapsed < 1*time.Second {
				t.Errorf("connection closed too fast (%v), expected ~2s hold time", elapsed)
			}
			return
		}
		_ = n
	}
	t.Error("speaker did not close connection after hold timer expiry")
}

// Test 12: Peer with wrong ASN → connection closed immediately.
func TestSpeaker_WrongASNRejected(t *testing.T) {
	t.Parallel()
	rm := newMockRM()
	_, addr, cancel := newTCPSpeaker(t, rm)
	defer cancel()

	conn, err := net.DialTimeout("tcp", addr.String(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Send OPEN with wrong ASN.
	wrongASN := uint32(99999)
	open := bgpmsg.NewBGPOpenMessage(
		uint16(min(wrongASN, 65535)),
		9,
		"1.2.3.4",
		[]bgpmsg.OptionParameterInterface{
			bgpmsg.NewOptionParameterCapability([]bgpmsg.ParameterCapabilityInterface{
				bgpmsg.NewCapFourOctetASNumber(wrongASN),
			}),
		},
	)
	if err := writeBGPMessage(conn, open); err != nil {
		t.Fatalf("write OPEN: %v", err)
	}

	// Speaker should close connection after ASN mismatch.
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 256)
	_, readErr := conn.Read(buf)
	if readErr == nil {
		t.Error("expected connection closed after wrong ASN, but read succeeded")
	}

	if rm.callCount() != 0 {
		t.Errorf("expected no route calls for wrong ASN peer, got %d", rm.callCount())
	}
}
