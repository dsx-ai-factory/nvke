<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Declaring infrastructure

An environment file describes hardware that exists. A qualification run names
one, asks it for a machine with certain capabilities, and gets back an instance
type the site actually offers.

Read this before adding a site, adding an instance type, or adding a second
provider. [hardware-lane.md](hardware-lane.md) covers what happens after a
machine is chosen.

Files live in `controller/test/e2e/environments/`, one per environment, named
by the value of `-e2e.environment`. Today there is exactly one, `forge.yaml`.

## The shape

Only the top level is common. Everything under `infrastructure` is the named
provider's own vocabulary, passed through the loader undecoded:

```yaml
provider: forge

infrastructure:          # opaque to the loader; the forge provider decodes it
  org:
    id: "…"
  sites:
    az61:
      id:            "…"
      vpcID:         "…"
      subnetID:      "…"
      sshKeyGroupID: "…"
      instanceTypes:
        - name: cpu.himem.t1
          id: "…"
          provides: {machineType: cpu, arch: amd64}
        - name: gpu.l40sx8
          id: "…"
          provides: {machineType: gpu, arch: amd64, gpu: l40}
```

That split is the point. A second provider describes its infrastructure in its
own terms, with its own field names, and nothing above the provider interface
changes. There is no shared schema for it to satisfy.

## How a machine gets chosen

A run asks for capabilities, not for a site or an instance type — those are
things only the provider knows exist:

```
-e2e.capabilities gpu=l40,arch=amd64
```

`MatchInstanceType` walks the declared types **in order** and returns the first
whose `provides` map satisfies every infrastructure capability requested. Three
keys are infrastructure and are matched on; anything else in the capability set
is ignored here, because it describes the software under test rather than the
machine:

| Key | Meaning |
|---|---|
| `machineType` | `cpu` or `gpu` |
| `arch` | `amd64`, `arm64` |
| `gpu` | the model, e.g. `l40`, `gb200` |

Two consequences worth internalising.

**Declaration order is the tie-break**, so declare cheapest first. Where
several types satisfy a request, the first listed wins.

**An empty request matches everything.** Asking for nothing is satisfied by
every type, so the first declared type wins by default. The Forge lane passes
no `-e2e.capabilities` today, which is why it always lands on `cpu.himem.t1`
and why the GPU type has never actually been selected.

A request nothing satisfies **fails rather than falling back**. Asking for
`{gpu: gb200}` at a site with no GB200 is an error, not a quiet substitution of
an L40. That is correct: the site genuinely cannot host the configuration.

## Identifiers, not names

Every field is the identifier the API takes, not the human-facing name. This is
a deliberate departure from the original design, which used names and resolved
them at run time.

The reason is concrete: **az61 and ytl both have a VPC named `rel-eng-tests`,
and they are different VPCs.** Names are unique per site, not per org, so
name-based lookup is ambiguous in exactly the case we would hit. Checking in
identifiers also removes a resolution API call from the hot path of every run.

The cost is that the file is unreadable without a lookup, which is what the
comments in `forge.yaml` are for.

## What is deliberately absent

**Credentials.** The Forge API key and the Teleport join are workflow secrets,
read from the environment and never written to this file, a values file, or a
command line.

**The jump host.** It is a repository variable rather than a declared field, so
the access path is not published alongside the site. Note the tension: VPC and
subnet identifiers *are* checked in, so the line between "identifier" and
"access path" is a judgement rather than a rule, and worth revisiting
deliberately.

**The org's human-facing name.** Recorded in a comment. The field lands when
something reads it, rather than being accepted and silently dropped by a
decoder that ignores unknown keys.

## What a key group does not give you

`sshKeyGroupID` governs **Serial-over-LAN console access**, not
`authorized_keys`. It grants no shell on a node.

Shell access comes from the ed25519 keypair the run generates per provision and
injects through cloud-init, reached through the jump host. The key group is
what lets you open a console on a node that is not answering ssh — invaluable
when triaging, and useless for anything the harness does normally.

## When the file changes

**Only when the hardware does.** A run needing a different Kubernetes version
or a different OS image touches nothing here; neither is an infrastructure
input. Those arrive as `-e2e.versions`.

## Adding a site

Two things will stop you, and both are worth knowing before you start.

**Network attachment differs by site.** az61 is `ETHERNET_VIRTUALIZER`, so an
interface attaches by subnet, which is what `subnetID` carries. ytl is FNN and
attaches by **VPC prefix** instead. The schema has no prefix alternative, so
**ytl cannot currently be declared.** Adding it means adding that arm to the
schema and to `interfaces()` in the provider.

This matters beyond ytl: ytl is where the GB200 capacity is. A stream asking
for `{gpu: gb200}` has nothing to run on until the prefix work is done.

**Enumerating instance types is awkward.** A Forge instance-type list caps at
twenty rows and ignores every filter, so discovering the rest means reading
them by id. `forge.yaml` declares only the two types that have actually carried
a workload at az61, rather than everything the site offers.

## Adding a provider

A second provider is a second implementation of `infra.Provider` plus an
environment file in its own vocabulary. Nothing above that interface changes,
and there is no schema to agree on — the loader only reads `provider` and hands
the rest over.

Capability matching is the provider's job rather than generic code, which is
what makes this work: a provider that has no concept of an "instance type" is
free to match a request however it likes.
