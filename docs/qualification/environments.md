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
by the value of `-e2e.environment`. There are two: `forge.yaml`, and `dsx.yaml`
for the DSX datahalls. One file per *provider* — a datahall or site is an
entry inside one, not a file of its own.

## The shape

Only the top level is common. Everything under `infrastructure` is the named
provider's own vocabulary, passed through the loader undecoded:

```yaml
provider: forge

infrastructure:          # opaque to the loader; the forge provider decodes it
  org:
    id: "${FORGE_ORG_ID}"
  sites:                 # named for the site, not the role: capabilities choose
    site-1:
      id:            "${FORGE_SITE_ID}"
      vpcID:         "${FORGE_VPC_ID}"
      subnetID:      "${FORGE_SUBNET_ID}"      # this fabric attaches by subnet
      sshKeyGroupID: "${FORGE_SSH_KEY_GROUP_ID}"
      instanceTypes:
        - name: cpu.himem.t1
          id: "${FORGE_INSTANCE_TYPE_CPU_ID}"
          provides: {machineType: cpu, arch: amd64}
        - name: gpu.l40sx8
          id: "${FORGE_INSTANCE_TYPE_GPU_ID}"
          provides: {machineType: gpu, arch: amd64, gpu: l40s}
    site-2:
      id:            "${FORGE_SITE_2_SITE_ID:-}"
      vpcID:         "${FORGE_SITE_2_VPC_ID:-}"
      vpcPrefixID:   "${FORGE_SITE_2_VPC_PREFIX_ID:-}"   # this one attaches by prefix
      sshKeyGroupID: "${FORGE_SITE_2_SSH_KEY_GROUP_ID:-}"
      instanceTypes:
        - name: cpu.arm
          id: "${FORGE_SITE_2_INSTANCE_TYPE_CPU_ARM64_ID:-}"
          provides: {machineType: cpu, arch: arm64}
        - name: gpu.gb200.h05.ibx4.dpux2
          id: "${FORGE_SITE_2_INSTANCE_TYPE_GPU_GB200_ID:-}"
          provides: {machineType: gpu, arch: arm64, gpu: gb200}
```

That split is the point. A second provider describes its infrastructure in its
own terms, with its own field names, and nothing above the provider interface
changes. There is no shared schema for it to satisfy.

## How a machine gets chosen

A run asks for capabilities, not for a site or an instance type — those are
things only the provider knows exist:

```
-e2e.capabilities gpu=l40s,arch=amd64
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
| `gpu` | the model, e.g. `l40s`, `gb200` |

Two consequences worth internalising.

**Declaration order is the tie-break**, so declare cheapest first. Where
several types satisfy a request, the first listed wins.

**An empty request matches everything.** Asking for nothing is satisfied by
every type, so the first declared type wins by default. The Forge lane always
passes `-e2e.capabilities`, built from the `arch` and `gpu` of the stream it runs, so this is
the shape a hand-run with neither falls back to.

A request nothing satisfies **fails rather than falling back**. Asking for
`{gpu: gb200}` at a site with no GB200 is an error, not a quiet substitution of
an L40. That is correct: the site genuinely cannot host the configuration.

## Identifiers, not names

Every field is the identifier the API takes, not the human-facing name. This is
a deliberate departure from the original design, which used names and resolved
them at run time.

The reason is concrete: **two of our sites have a VPC of the same name, and
they are different VPCs.** A VPC name is unique per site rather than per org,
so name-based lookup is ambiguous in exactly the case we would hit. Using
identifiers also removes a resolution API call from the hot path of every run.

## Where the identifiers come from

Not from the file. Every `${NAME}` above is read from the process environment
when the environment loads.

A plain `${NAME}` is required: unset or empty fails the run before anything is
created, and every missing name is reported at once. `${NAME:-}` defaults to
empty instead, which is why site-2's identifiers take that form. A site missing
any of them does not validate, and `Resolve` skips it rather than failing, so a
repository that has not onboarded site-2 still runs against site-1. Set them to
qualify at site-2.

CI supplies them from repository variables, which is the same route
`FORGE_TELEPORT_PROXY` and `FORGE_TELEPORT_JOIN_TOKEN` take -- the latter
names a Teleport provision-token resource rather than being a credential
itself, so a variable is enough. `FORGE_JUMP_HOST` is a secret instead, so
the job log masks it, and it is scoped to the `forge` GitHub environment
rather than the repository, alongside `FORGE_API_KEY` and
`FORGE_SITE_2_JUMP_HOST`.

The split is between engineering and account. Which sites exist, which machine
shapes they offer, and what those shapes `provides` is engineering: it is
reviewed, it explains why a run matched the machine it did, and it belongs in
the file. An org, site, VPC or subnet id is not — it names one organization's
real infrastructure, and this repository is the staging ground for a public
one.

To run against the site from a laptop, export the variables first:

```bash
export FORGE_ORG_ID=… FORGE_SITE_ID=… FORGE_VPC_ID=… FORGE_SUBNET_ID=…
export FORGE_SSH_KEY_GROUP_ID=… FORGE_INSTANCE_TYPE_CPU_ID=… FORGE_INSTANCE_TYPE_GPU_ID=…
```

Their values are on the repository's Actions variables page. Unit tests need
none of this: they substitute synthetic values, so
`TestCheckedInEnvironmentsParse` still checks that this file is well-formed
wherever it runs.

## What is deliberately absent

**Credentials.** The Forge API key is a workflow secret, read from the
environment and never written to this file, a values file, or a command line.
The Teleport join token is a variable, not a secret -- it only names a
Teleport provision-token resource; the actual credential is the job's GitHub
OIDC token, which Teleport validates server-side.

**The jump host.** It is a workflow secret rather than a declared field, so
the access path is not published alongside the site, and not a repository
variable like the org, site, VPC and subnet identifiers -- GitHub prints
job-level `env` in every step's log header, and only a secret is masked
there. Keeping them as `${NAME}` references rather than literals was raised
in review and settled deliberately; a site added later declares its own
references under `sites:` and the variable list grows with it.

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

One thing will stop you, and it is worth knowing before you start.

**Network attachment differs by site.** A fabric attaches an interface by
subnet or by **VPC prefix**, never both, and which one is a property of the
fabric rather than a choice. A site declares `subnetID` or `vpcPrefixID`
accordingly, and declaring both is rejected on lookup. site-1 attaches by
subnet, site-2 by prefix.

**Enumerating instance types is awkward.** A Forge instance-type list caps at
twenty rows and ignores every filter, so discovering the rest means reading
them by id. `forge.yaml` declares only the two types that have actually carried
a workload, rather than everything the site offers.

## Adding a provider

A second provider is a second implementation of `infra.Provider` plus an
environment file in its own vocabulary. Nothing above that interface changes,
and there is no schema to agree on — the loader only reads `provider` and hands
the rest over.

`dsx.yaml` is the worked example, and it is worth reading beside `forge.yaml`
for how little the two have in common below `provider:`:

```yaml
provider: dsx

infrastructure:
  org:
    id: "${DSX_ORG_ID:-ncx}"          # a name, where Forge's is numeric
  sites:
    datahall-1:                       # generic; `api` carries the host
      api:         "${DSX_DATAHALL_1_API:-}"
      id:          "${DSX_DATAHALL_1_SITE_ID:-}"
      tenantID:    "${DSX_DATAHALL_1_TENANT_ID:-}"   # no Forge equivalent
      vpcPrefixID: "${DSX_DATAHALL_1_VPC_PREFIX_ID:-}"
      instanceTypes:
        - name: datahall-1-nvke-tenant
          id: "${DSX_DATAHALL_1_INSTANCE_TYPE_ID:-}"
          provides: {machineType: gpu, arch: amd64}
```

Three differences are load-bearing rather than cosmetic.

**Each datahall declares its own API host.** `api` is required and there is no
site-lookup call the way Forge needs one — the host addresses the datahall
directly. The key itself is generic, as `forge.yaml`'s `site-1` and `site-2`
are, so which datahall this is stays in a variable. Two datahalls at one site
hold completely disjoint UUIDs for the same kinds of object, and nothing in a
response says which one an id came from — so a second datahall is a second key,
never a reused one.

**Credentials are per datahall.** The names are derived from the key:
`DSX_<DATAHALL>_SSA_CLIENT_ID` and `_SECRET`, exchanged for a bearer token at
the issuer in `SSA_ISSUER`. A client scoped to one datahall cannot be picked up
by a run against another, and a second datahall needs no code.

**`full` runs, and picks its own tenant.** Every stage runs on DSX: the
datahall has its own tenant spec, so a `full` run builds the site and then a
control plane and worker on it. The site lane registers one tenant entry per
dialect and each decodes the environment strictly, so the lane excludes the
entries belonging to other providers by label --- `site-controller &&
!nico-forge` here --- rather than requiring its own, which would also drop
every site-level spec. A provider added later needs an exclusion on the other
lanes as well as its own; the mapping is one `case` block in `qualify-e2e.yml`.

Capability matching is the provider's job rather than generic code, which is
what makes this work: a provider that has no concept of an "instance type" is
free to match a request however it likes.
