<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Running the hardware lane

The hardware lane is the e2e suite's `site-controller` specs, run on real
Forge machines. A run provisions a bare-metal node, bootstraps Kubernetes on
it with `nvkectl`, installs the NVKE stack, creates a tenant cluster through
the controller, checks the site and the tenant, then deletes the tenant and
releases every machine. It is the only lane that runs on real hardware, so it
is the one that shows a candidate works on the path a customer takes.

This is the operator's guide: how to start a run, follow it, and read what it
leaves behind. [hardware-lane-design.md](hardware-lane-design.md) explains how
the lane works inside, and [triage.md](triage.md) covers a failed run.

## Ways to run it

| Route | Use it for | What it qualifies | What you need |
|---|---|---|---|
| [Dispatch the workflow on `main`](#dispatch-the-workflow) | qualifying a version: a beta, a release candidate, a combination | the published artifacts the plan names, with the suite from `main` | write access to this repository, and an approval |
| [Label a pull request](#label-a-pull-request) | a change on a branch of this repository | the same, with the suite and workflow from the pull request | triage access, and an approval |
| [Push a beta tag](#push-a-beta-tag) | the nightly beta | the commit the tag marks | permission to push the tag |
| [`qualify local` from your fork](#run-it-from-your-fork-with-qualify-local) | a change from a fork, or work on the lane itself | your checkout, built from source, with the OS image from the plan | a Teleport login, and the Forge site's values and API key |

The workflow does not run for a pull request from a fork: a fork cannot reach
the Forge API key or the Teleport join. `qualify local` runs the workflow's
own steps, from the same script, with your credentials instead, so a fork can
produce the same evidence.

## Before you start

**A run holds real machines.** A `full` run holds three for about an hour:
the site controller, a tenant control plane and a tenant worker. Site-2 has
room for three runs at once, and a fourth makes every run in flight there fail
at provisioning. Check the
[workflow's runs](https://github.com/dsx-ai-factory/nvke/actions/workflows/qualify-e2e.yml)
before starting one.

**Never kill a run once the suite has started.** Teardown happens inside the
suite. A run that is cancelled, killed, or hits its timeout leaves its
machines allocated, and someone has to delete them in Forge by hand. A run
that keeps its machines on purpose (`keep-site`) needs the same cleanup.

## Run it in CI

The `qualify-e2e` workflow has three jobs. `plan` runs `qualify plan` and
uploads `plan.json`. `qualify-run` runs once for each stream in the plan's
scope, named for its stream. `results` adds a card to the run page.

**Every run waits for approval**, dry runs included, unless it was started by
a beta tag. On the run page choose **Review deployments**, select `forge`, and
approve. A reviewer may approve a run they started.

### Dispatch the workflow

From the [workflow page](https://github.com/dsx-ai-factory/nvke/actions/workflows/qualify-e2e.yml)
choose **Run workflow** on `main`, or from a shell:

```bash
gh workflow run qualify-e2e.yml --repo dsx-ai-factory/nvke --ref main -f scope=l40s -f stage=full
```

Start with a dry run. It plans, checks every artifact, and prints the suite
invocation without taking a machine:

```bash
gh workflow run qualify-e2e.yml --repo dsx-ai-factory/nvke --ref main -f scope=l40s -f dry-run=true
```

A dispatch only works from `main`. The `forge` environment, and the Teleport
join behind it, refuse any other branch.

The inputs you usually set:

| Input | Default | What it does |
|---|---|---|
| `scope` | `golden-path` | Which streams to run, one job each. See [Scopes](#scopes). |
| `stage` | `full` | How far to go. See [Stages](#stages). |
| `dry-run` | `false` | Plan and check without taking a machine. |
| `nvke-version` | empty | The `nvke` tag to qualify, such as `v0.22.0-beta.1`. Empty takes `QUALIFY_E2E_NVKE_VERSION`, then the highest tag. |
| `resolution` | `commitDescribe` | `commitDescribe` describes each component at the commit `nvke-version` points at, which is how a beta is qualified. A release qualification sets `gitTag`. |
| `keep-site` | `false` | Keep the site node, and a failed tenant's nodes, to debug. Delete them by hand afterwards. |
| `image-registry` | `nvcr` | Where the controller and capi-provisioned images are pulled from: `nvcr` or `ghcr`. |

The remaining inputs each override one version the plan resolved: the
controller, nvkectl, capi-provisioned, Kubernetes, the OVN CNI, the OS image
and its URLs, the deploy path's commit, and the controller's install name.
The dispatch form describes each. Leave them empty unless you are qualifying a
specific combination.

**What a CI run builds and what it pulls.** The run pulls the published
artifacts the plan names. It builds the controller and nvkectl from the
checkout only when the plan names no version for them, and always pulls
capi-provisioned. Before it takes a machine it checks that every artifact is
published, waiting up to 90 minutes for a plan resolved at a commit. The
resolved inputs, each with where it came from, are in the job log and in
`run-inputs.txt`.

### Label a pull request

Add a label to a pull request from a branch of this repository:

| Label | Runs |
|---|---|
| `qualify-e2e` | the scope in `QUALIFY_E2E_SCOPE`, or `golden-path` when that is unset |
| `qualify-e2e-l40s` | the `l40s` scope |
| `qualify-e2e-dsx` | the `dsx` scope |
| `qualify-e2e-dry-run` | a dry run |

A pull request has no dispatch inputs, so a labelled run takes them from the
`QUALIFY_E2E_*` repository variables. Pushing to the pull request does not
start another run. To run again, remove the label and add it back, or use
**Re-run jobs**.

### Push a beta tag

Pushing a `vX.Y.Z-beta.N` tag qualifies the commit it marks: scope
`golden-path`, stage `full`, `resolution: commitDescribe`, pulling from NVCR.
It ignores the `QUALIFY_E2E_*` variables for scope, stage, keep-site and
registry, but two still apply: `QUALIFY_E2E_SITE_CONTROLLER_VERSION` installs
another controller in place of the plan's, while the result still records the
plan's, and `QUALIFY_E2E_DEPLOY_PATH_REF` installs another deploy path. Unset
both after the label run that needed them. `QUALIFY_E2E_NGC_ORG` and
`QUALIFY_E2E_NODE_USER` apply by design. It runs in the `forge-nightly`
environment, which needs no approval. A beta is cut once a night.

### What a CI run leaves behind

- **The results card** on the run page: a row for each stream, then its specs
  and the failure for anything that did not pass.
- **An artifact for each stream**, `forge-site-<stage>-<stream>-<run id>-<attempt>`,
  kept for 7 days. It holds the Ginkgo log, the JUnit report,
  `run-inputs.txt`, `qualify-result.json`, the Teleport bot's log, and on a
  failure the diagnostics the suite collected. The jump host is withheld from
  these files before upload. [triage.md](triage.md#start-with-the-artifacts)
  says what each file is for.
- **TestGrid.** A run from `main` or a tag publishes its result there; a pull
  request's run does not.

## Run it from your fork with `qualify local`

`qualify local` runs one stream through the workflow's steps from your own
clone: plan, check, build, provision, suite, teardown, record. It uses the
same lane script as the workflow, so the steps and flags are CI's.
[local-runs.md](local-runs.md) is its full reference: every flag, every
variable, and where it differs from CI.

### 1. Get the site's values

Ask the lane's maintainers for them; none is in the repository. Keep them in
a file only you can read, outside the clone:

```bash
# ~/nvke-forge.env (chmod 600)

# The same for everyone.
export FORGE_TELEPORT_PROXY=<teleport proxy>
export QUALIFY_E2E_NGC_ORG=<ngc org>
export FORGE_ORG_ID=<org id>
export FORGE_SITE_ID=<site-1 id>
export FORGE_VPC_ID=<site-1 vpc id>
export FORGE_SUBNET_ID=<site-1 subnet id>
export FORGE_SSH_KEY_GROUP_ID=<site-1 key group id>
export FORGE_INSTANCE_TYPE_CPU_ID=<site-1 cpu type id>
export FORGE_INSTANCE_TYPE_GPU_ID=<site-1 gpu type id>

# Yours: the jump host, and one of the Logins `tsh status` lists.
export FORGE_JUMP_HOST=<jump host>
export FORGE_TELEPORT_LOGIN=<login>

# Secret.
export FORGE_API_KEY=<forge api key>
```

A GB200 stream runs at site-2 and also needs the `FORGE_SITE_2_*` values
listed in [local-runs.md](local-runs.md#the-simplest-case). You need `go`,
`bash`, `jq`, `git`, `make`, `docker` and `tsh` installed.

### 2. Clone your fork

```bash
git clone https://github.com/<you>/nvke.git
cd nvke
git remote add upstream https://github.com/dsx-ai-factory/nvke.git
git checkout <your branch>
```

`qualify local` plans from upstream's tags, and finds the upstream remote by
its URL, whatever it is called.

### 3. Run

From the repository root:

```bash
source ~/nvke-forge.env
tsh login --proxy="$FORGE_TELEPORT_PROXY" --ttl=720
go run ./release/cmd/qualify local --stream forge-l40s-amd64-ubuntu-24.04-kube-1.36 --stage full --from-source
```

`--from-source` builds the controller, nvkectl and capi-provisioned from your
working tree, uncommitted changes included, so no registry login is needed.
The OS image, and the Kubernetes and OVN packages inside it, are the plan's
published builds.

Before it takes a machine, it checks the tools, the variables, the Teleport
login and every artifact the run needs, and stops naming whatever is missing.
Add `--dry-run` to stop after those checks and print the suite's arguments.
The Teleport login has to last the whole run, teardown included, which is why
`--ttl` is long.

To stop a run, press Ctrl-C once and wait while Ginkgo tears the machines
down. `qualify local` does not pass a second Ctrl-C on, because Ginkgo would
skip its cleanup.

### 4. Read the result

`qualify local` prints its run directory when it starts: a new directory under
`~/Library/Caches/nvke/qualify-local/` on macOS, or
`~/.cache/nvke/qualify-local/` on Linux.

| File | What it is |
|---|---|
| `qualify-result.json` | The outcome, and the versions that ran, with built ones named by commit |
| `qualify-e2e.log` | The suite's log |
| `qualify-e2e.junit.xml` | The JUnit report |
| `plan.json`, `plan-as-run.json` | The plan, and the plan with each built component named by the commit it was built at |

The jump hosts are scrubbed from these files. `qualify-result.json` names no
host or address, so it is the file to attach to a pull request; the others
still carry the site's addresses and ids.

### Other streams and options

| To | Change |
|---|---|
| Run L40S on Ubuntu 26.04 | `--stream forge-l40s-amd64-ubuntu-26.04-kube-1.36` |
| Run GB200 with an arm64 site controller | `--stream forge-gb200-arm64-ubuntu-24.04-kube-1.36`, with the site-2 values |
| Run GB200 with an amd64 site controller | `--stream forge-gb200-amd64-ubuntu-24.04-kube-1.36`, with the site-2 values |
| Stop at an earlier stage | `--stage provision` or `--stage bootstrap` |
| Qualify a specific version | `--version v0.22.0-beta.1` |
| Reuse an earlier run's plan | `--plan <run directory>/plan.json` |
| Pull published images instead of building | drop `--from-source`, and log in to a registry; see [local-runs.md](local-runs.md#registry) |
| Keep the machines to debug | `--keep-site` or `--keep-tenant`; delete them in Forge afterwards |

## Scopes

A scope selects streams from `release/policy.yaml`. In CI each stream is one
job; `qualify local` runs the one stream you name.

| Scope | Streams |
|---|---|
| `golden-path` | Ubuntu 24.04 on GB200, amd64 and arm64 site controllers, at site-2 |
| `l40s` | Ubuntu 24.04 on L40S, amd64, at site-1 |
| `dsx` | the DSX datahall |
| `full` | every Forge stream: GB200 and L40S on Ubuntu 24.04 and 26.04 |

## Stages

| Stage | Stops after | What a pass shows |
|---|---|---|
| `provision` | the node boots and answers over SSH | Forge gives us a machine running the OS image under test |
| `bootstrap` | `nvkectl bootstrap site` | `nvkectl` builds Kubernetes on this hardware |
| `deployment` | the NVKE stack is installed and the controller answers | the deploy path installs, and the controller reports the expected version |
| `full` | a tenant cluster is created, checked and deleted | the product creates a tenant cluster on real machines |

`qualify local` takes all four, and refuses any other value before it builds
anything.

## What a `full` run does

| Step | Spec | Time on recent passing runs |
|---|---|---|
| Provision the site controller node | `provisions a node` | about 12 minutes |
| Bootstrap Kubernetes | `bootstraps the site controller` | about 4 minutes |
| Install the NVKE stack | `deploys the stack` | about 3 minutes |
| Check the site | `delivers the site's telemetry through NKTP`, `has no pod failing in any namespace` | about 2 minutes |
| Create the tenant cluster | `creates the cluster` | about 15 minutes |
| Check the tenant | `runs every workload …`, `has no pod failing …` | about 3 minutes |
| Delete the tenant, then release the site node | `deletes the cluster`, then teardown | a few minutes |

The suite took about 45 minutes on the L40S and GB200 runs that passed in late
October 2026, plus setup and builds.

**Following a run.** In CI, open the stream's `qualify-run` job and its **Run
the e2e suite** step. With `qualify local`, the output is in your terminal.
Either way the log names each spec as it starts, and during provisioning it
can be quiet for ten minutes or more.

**What a pass looks like.** `qualify-result.json` reads `passed`,
`51/80 passed`. The label filter selects 80 specs on a Forge `full` run, and
29 of them skip because they check for components this lane does not
install, so 51 of 80 is a full pass.

## When a run fails

1. **Find where it stopped.** A failure before the suite starts is setup:
   approval, versions, artifacts, the Teleport login, or a missing variable.
   No machine has been taken, and the error names what was missing.
2. **For a failure in the suite**, follow [triage.md](triage.md) with the log
   and the diagnostics. It tells a site fault (capacity, DHCP, the metadata
   endpoint) from a defect in our code.
3. **Check for machines left behind.** Teardown releases them on a pass and
   on a failure, but not after a cancel, a kill, a timeout, or a keep option.
   The log line `tenant sweep failed; releasing the site anyway` names
   tenants whose machines need deleting by hand.
   [triage.md](triage.md#capacity) shows how to list a site's instances.
4. **Run again.** In CI, use **Re-run failed jobs** or re-add the label. With
   `qualify local`, run it again; `--plan` with the last run's `plan.json`
   skips planning.

## What a pass does not prove

- **GPU workloads.** GPU workers provision, join and release, but nothing
  installs a GPU operator, so no GPU workload runs.
- **Site controller upgrade and scale.** There are no specs for either.
- **Deeper node-pool behaviour.** The replacement, cascade and scale-down
  specs run on the simulator lanes, not on this one.

[hardware-lane-design.md](hardware-lane-design.md#what-this-lane-does-not-cover)
has the detail.
