# Production incident model vs local capd-bgp

This document explains how the production kube-vip/BGP traffic path maps to the
local `capd-bgp` helper. The local setup is useful for testing BGP route
advertisement and withdrawal, but it does not faithfully reproduce the
production DPU/HBN ARP-neighbor failure mode.

## Production participants

| Production part | Role |
|-----------------|------|
| Worker host | Sends traffic to the Kubernetes API VIP. |
| Worker DPU / HBN | Host networking dataplane. Owns bridge interfaces, routes, ARP/neighbor state, and forwarding policy for the worker side. |
| kube-apiserver host | Control-plane host running kube-apiserver and kube-vip. |
| Control-plane DPU / HBN | Host networking dataplane for the control-plane side. |
| kube-vip | BGP-speaking process on the control-plane host. Advertises or withdraws the API VIP route. |
| ToR switch/router | Physical network device. Runs its own BGP process and peers with kube-vip. |
| Fabric | The routed network beyond one host or rack, commonly ToR/leaf/spine switching. |
| Local subnet | IP prefix directly attached to an interface. ARP/neighbor learning should normally be limited to this scope. |
| VIP range/subnet | Routed service range containing the API VIP. It should be reached by routing/BGP, not by stale local ARP learning. |

The ToR is the BGP peer from kube-vip's perspective. More precisely, both sides
run BGP-speaking processes:

```text
kube-vip on CP host  <---- BGP session ---->  ToR routing process
     BGP speaker                              BGP speaker / peer
```

## Healthy production route

In the healthy case, kube-vip advertises the API VIP to the ToR. Worker traffic
uses normal routing through the DPU/HBN and fabric to reach whichever
control-plane host currently advertises the VIP.

```text
Production, healthy path
========================

  Worker host subnet                         Control-plane host subnet
  10.20.1.0/24                               10.10.1.0/24

  +----------------------+                   +---------------------------+
  | Worker Host          |                   | kube-apiserver Host       |
  | src: 10.20.1.45      |                   | host IP: 10.10.1.21       |
  | dst: VIP 10.99.0.10  |                   | VIP:     10.99.0.10       |
  +----------+-----------+                   |                           |
             |                               | +-----------------------+ |
             |                               | | kube-vip              | |
             |                               | | BGP speaker           | |
             |                               | +-----------+-----------+ |
             |                               +-------------|-------------+
             |                                             |
             v                                             | BGP advertise:
  +----------------------+                                 | 10.99.0.10/32
  | Worker DPU / HBN     |                                 v
  | routes + ARP cache   |                   +---------------------------+
  +----------+-----------+                   | ToR switch/router         |
             |                               | BGP process / peer        |
             | route lookup                  | route: VIP -> CP host     |
             v                               +-------------+-------------+
  +----------------------+                                 |
  | Network fabric       | <-------------------------------+
  | ToR / leaf / spine   |
  +----------------------+
```

The intended forwarding decision is:

```text
Worker sends packet to VIP
  |
  v
Worker DPU/HBN checks whether VIP is local to the worker subnet
  |
  | VIP is not local
  v
Use L3 routing toward the fabric
  |
  v
ToR/fabric follows the BGP route learned from kube-vip
  |
  v
Packet reaches a valid kube-apiserver host
```

## Why subnets matter

A subnet is the IP range an interface treats as directly attached. For example:

```text
Worker interface IP: 10.20.1.45/24
Worker local subnet: 10.20.1.0/24

Control-plane host IP: 10.10.1.21/24
Control-plane subnet: 10.10.1.0/24

API VIP: 10.99.0.10/32 or part of a routed VIP range
```

ARP/neighbor learning is for finding a MAC address on a directly attached L2
segment. If the worker-side interface is on `10.20.1.0/24`, then learning an
ARP entry for a routed VIP like `10.99.0.10` is dangerous because that VIP is not
supposed to be treated as a local L2 neighbor.

```text
Expected:
  10.20.1.x addresses can be local ARP neighbors on the worker subnet.
  10.99.0.10 should be reached through routing/BGP.

Dangerous:
  HBN learns:
    10.99.0.10 -> aa:bb:cc:dd:ee:ff

  That turns a routed VIP into a local-looking L2 neighbor.
```

This affects blast radius. With subnet checks enabled, bad ARP learning is
limited to addresses that belong on the local interface subnet. Without those
checks, the dataplane can learn neighbor entries for routed VIPs, other tenant
ranges, or addresses outside the local segment. That expands the failure from a
local-neighbor mistake into a routed-service or cross-tenant forwarding problem.

## Broken production route

The incident shape was not simply "BGP failed to withdraw." The dangerous state
was a stale neighbor entry in the DPU/HBN dataplane.

```text
Production, broken path
=======================

  +----------------------+
  | Worker Host          |
  | dst: VIP 10.99.0.10  |
  +----------+-----------+
             |
             v
  +----------------------+
  | Worker DPU / HBN     |
  |                      |
  | Bad neighbor entry:  |
  | 10.99.0.10 -> old MAC|
  +----------+-----------+
             |
             | stale L2 forwarding decision
             v
  +----------------------+
  | stale / wrong target |
  | old CP or tenant     |
  +----------------------+

  Meanwhile, the routing control plane may already be correct:

  kube-vip withdraws old route
        |
        v
  ToR/fabric removes old BGP path
        |
        v
  But HBN still has stale VIP -> MAC state
```

The failure sequence looks like:

```text
1. kube-vip previously owned and advertised the VIP.
2. HBN learned a neighbor entry for the VIP, even though the VIP was outside
   the local subnet.
3. The control-plane host stopped owning the VIP.
4. kube-vip/BGP route withdrawal could be correct.
5. The stale neighbor entry still pointed VIP traffic at the old MAC.
6. Gratuitous ARP could refresh the stale entry before it expired.
7. New packets could keep landing on the wrong instance and reset TCP sessions.
```

The Carbide-side config fix is about preventing invalid neighbor learning:

```text
neighmgrd:
  subnet_checks=1

ifupdown2 policy:
  l3_intf_arp_accept=0
```

That is a forward-looking guard. If a bad neighbor entry already exists in the
kernel/dataplane, it may also need to expire or be flushed.

## Local capd-bgp equivalent

The local helper replaces the production ToR/fabric with a local FRR container
running in host network mode.

```text
Local capd-bgp layout
=====================

  Docker kind subnet
  usually 172.18.0.0/16

  VIP range
  default 172.31.0.0/24

  +---------------------------+        BGP session        +-------------------+
  | kind control-plane node   | <-----------------------> | capd-tor          |
  |                           |                           | FRR container     |
  | +-----------------------+ |                           |                   |
  | | kube-vip              | |                           | +---------------+ |
  | | BGP speaker           | |                           | | FRR bgpd      | |
  | +-----------------------+ |                           | | BGP speaker   | |
  +-------------+-------------+                           | +-------+-------+ |
                |                                         |         |         |
                | Docker kind bridge                      |         v         |
                v                                         | FRR zebra         |
  +---------------------------+                           +---------+---------+
  | kind worker node          |                                     |
  | dst: controlPlaneVIP      |                                     |
  +-------------+-------------+                                     |
                |                                                   |
                | default gateway: kind bridge gateway              |
                v                                                   v
  +---------------------------+                           +-------------------+
  | laptop/Colima host kernel | <------------------------- | host route table  |
  | route lookup for VIP      |     route installed by FRR | VIP -> CP nexthop |
  +---------------------------+                           +-------------------+
```

The local forwarding decision is:

```text
kind worker sends packet to controlPlaneVIP
  |
  v
VIP is outside the kind subnet, so worker uses its default gateway
  |
  v
Default gateway is the kind bridge gateway in the host namespace
  |
  v
FRR zebra installed a BGP-learned host route on the host
  |
  v
Host routes packet to one of the advertising control-plane containers
```

When a control-plane node withdraws the VIP:

```text
CP kube-vip stops advertising VIP/32
  |
  v
FRR bgpd removes that CP path
  |
  v
FRR zebra updates the host kernel route
  |
  v
New worker packets avoid the withdrawn CP nexthop
```
