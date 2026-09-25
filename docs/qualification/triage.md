<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Triaging a failed hardware run

The first question on a red Forge lane is not "what broke" but **"is this our
defect or the site's?"** Of the first three real runs, two failed on
infrastructure and one on a code defect, and each took hours to tell apart.
This is how to do it faster.

[hardware-lane.md](hardware-lane.md) describes what the lane does. This
describes what to do when it stops doing it.

## Start with the artifacts

Every run uploads these whether it passed or failed (`if: always()`):

| Artifact | What it tells you |
|---|---|
| `site-e2e.log` | The Ginkgo run. Start here. |
| `site-e2e.junit.xml` | The same result, machine-readable. |
| `run-inputs.txt` | Which versions, images and settings the run actually resolved — including which were overridden. Answers "was it even testing what I thought?" |
| `site-diagnostics.tar.gz` | What `nkectl` collected **on the node**. Present only when nkectl wrote a diagnostics directory, which it does when it fails. |
| `tbot.log` | The Teleport bot. Only interesting when nothing could be reached at all. |

**An absent `site-diagnostics.tar.gz` is normal and is information.** `nkectl`
writes its diagnostics directory when it fails, so no tarball usually means
bootstrap succeeded and the run failed somewhere after it. The suite
distinguishes this case (`ErrNoDiagnostics`) from a real failure to archive,
because reporting "could not collect diagnostics" for the ordinary case sends
the reader hunting a second fault that does not exist.

## Which layer failed

Work down. Each layer has a different owner.

### The run never reached the hardware

Symptoms: failure before any instance id appears in the log; `tbot.log`
carries the detail.

Almost always the Teleport join or the bot certificate, not Forge. The runner
joins with the repository's GitHub OIDC token and nothing is stored, so a
failure here is a trust-relationship problem rather than a credential that
expired.

### Provisioning: the instance never became usable

The provider waits twice, and the two waits fail differently.

**`not ready after 30m`** — Forge never brought the instance up, or brought it
up without an address. Ours to report, not to fix.

**`not reachable through <jump host> after 20m`** — Forge says Ready but sshd
never answered. This is the interesting one, because "Ready" means Forge handed
the machine over, not that the image was written and booted. The gap covers
the entire iPXE boot, disk write and first boot.

**cloud-init did not finish cleanly** — the node booted and answered, but
`cloud-init status --wait` returned non-zero. The log carries its output. Node
preflight rejects any cloud-init status other than `done`, so the run stops
here deliberately rather than failing later and less clearly.

### The DHCP fault, and how to recognise it

Seen on run 36066151933, tenant control-plane node `cp-4342`, stuck
`Configuring` for over 70 minutes. The signature, from the Serial-over-LAN
console:

- the interface is **up with carrier** — 200Gbps, LLDP peer visible — and has
  **no IP address**;
- `systemd-networkd` logged `Configuring with ...` and then nothing at all;
- `systemd-networkd-wait-online` failed;
- cloud-init could not reach `169.254.169.254` ("Network is unreachable"), fell
  back to `DataSourceNone`, and so set no hostname and installed no packages;
- `kubelet`, `containerd` and `nke-agent` are all `inactive`, and the disk is
  nearly empty.

Carrier up and no address is the tell. That is DHCP, which is the site's, and
no amount of retrying from our side fixes it. File it against Forge with the
instance id, the MAC, and the switch the LLDP peer names — for that run,
instance `eac4f3aa-8c0a-4b54-ae47-c5966d47ee6a`, MAC `5c:25:73:79:fe:ba`,
switch `10-91-69-85.az61.frg.nvidia.com`.

Reaching the console at all goes through the site's SSH key group, which
governs Serial-over-LAN and not `authorized_keys`. It gives you a console on a
node you cannot ssh to, which is exactly the case that matters here.

### The metadata endpoint, which is broken by design right now

Since 2026-09-19 new instances cannot reach `169.254.169.254`. The lane works
around it with `phoneHomeEnabled=false`, because phone-home is what normally
flips an instance to Ready and the callback can never complete.

Two consequences when reading a failure:

- An instance that sits in `Provisioning` forever, on a path where we did *not*
  set the flag, is this fault and not a new one.
- Because the flag is off, **Ready no longer implies cloud-init finished**.
  That is why the provider separately waits on `cloud-init status --wait`, and
  why a node can be Ready and useless at the same time.

### Bootstrap: the node came up, the cluster did not

`site-diagnostics.tar.gz` exists. Read it first — it is nkectl's own account of
what it was doing.

The two failure modes worth knowing in advance:

- **An opaque TLS handshake error on every dial.** The node agent's
  `AGENT_ARGS` and `nkectl bootstrap site` disagree about mTLS. They have to
  match: `--disable-mtls` on one requires it on the other.
- **The cluster sits in Provisioning until the timeout, reporting nothing.**
  On the default agent args the agent expects a SPIFFE identity, which it can
  only get if the site spec carries the node's TPM EK thumbprint. Without one,
  nkectl deploys no TPM attestor and the agent never attests.

### Tenant: the cluster was created and something could not reach it

If a tenant spec fails on a dial or a timeout rather than an assertion, check
whether the endpoint relay engaged. A working run logs it:

```
relaying the tenant API server  published="10.91.194.69:6443"  local="127.0.0.1:45011"
```

No such line, plus a timeout dialling a `10.x` address, means the relay did not
run and the runner tried to route to an address only the site can reach. That
was run 36051890151.

## Slow is not the same as hung

Two places where a healthy run looks stuck:

**Delete takes 20 to 40 minutes.** Forge does not release a machine promptly,
and a tenant cluster is not gone until both of its machines are. The measured
figure on the green run was 40.5 minutes against a 45-minute budget. Instances
reporting `Terminating` are making progress. Run 36051890151 failed its delete
at exactly 1800.306s against a 30-minute budget that was simply too small.

**Provisioning takes most of an hour before anything interesting happens.** The
green run took 81 minutes end to end. A lane that has been running 20 minutes
with a quiet log has not failed.

## Capacity

az61 offers 8 `cpu.himem.t1` slots and a `full` run needs 3: one site node, one
tenant control plane, one tenant worker. A create that fails immediately with a
capacity error, rather than timing out, means the slots are taken — often by
instances from an earlier run that are still `Terminating`.

Check before assuming a defect:

```
GET /v2/org/<org id>/forge/instance?siteId=<site id>
```

It returns a bare JSON list, not an object with an `instances` key.

## Before filing against Forge

Have these ready, all of which the artifacts carry:

- the instance id, and the run id;
- which of the two waits failed, and after how long;
- what the console showed, if it was reachable — carrier, address, and whether
  `systemd-networkd` said anything after `Configuring`;
- whether cloud-init reached the metadata endpoint.

A report naming the instance, the MAC and the switch is actionable. "The node
did not come up" is not.
