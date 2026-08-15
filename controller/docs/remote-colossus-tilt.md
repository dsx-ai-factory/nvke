# Remote Colossus Tilt (Mac → Linux)

Edit NKE repos on your Mac and run the CWE / forge-vms Tilt harness on a remote
x86_64 Linux host (typically a Colossus bare-metal lease). [Mutagen](https://mutagen.io)
mirrors source and forwards UIs / kubectl back to the Mac.

Use this when Apple Silicon / macOS cannot run the full forge-vms path (amd64 OS
profile + KVM/libvirt). For laptop-local CAPD/Tilt without real VMs, see the
main [README](../README.md#capi--tilt-development-recommended).

## Before you start

This guide assumes you already have a remote Linux box you can SSH into
(typically a Colossus lease) with forge-vms / KVM host setup done — Ubuntu
24.04 x86_64, `/dev/kvm`, Docker, kind, tilt, kubectl, Go, `ko`, clusterctl,
and private GitLab Go auth (`GOPRIVATE=gitlab-master.nvidia.com` plus netrc or
SSH `insteadOf` so detached `tilt` builds work without agent forwarding).

Getting that host in the first place is covered by the
[`provision-colossus-local-forge`](https://gitlab-master.nvidia.com/nke/nke-agent-skills/-/tree/main/skills/provision-colossus-local-forge)
skill in `nke-agent-skills`. Use that (or an equivalent manual setup), then come
back here.

On the Mac you need:

- Mutagen: `brew install mutagen-io/mutagen/mutagen`
- `yq` (for `fetch-kubeconfig.sh --update-mutagen`)
- An SSH config Host alias that reaches the box (e.g. `colossus`)
- Local checkouts of at least: `cloud-workflow-engine`,
  `cluster-api-provider-nico`, `agent`, `nke-site-deploy`, and `forge-console`
  (sibling layout under one parent dir is assumed; `nke-site-deploy` supplies
  `cwe-profiles` for the Tilt profiles image; `forge-console` is loaded by CWE
  Tilt from `../forge-console` or `FORGE_CONSOLE_DIR`)

## Mental model

- **Mac = source of truth.** Edit and run all `git` here.
- **Target host = test harness.** Runs Tilt (kind + CAPI/CAPD + CWE +
  forge-vms). Tilt must run there because builds need the host Docker/kind and
  KVM/libvirt.
- **Mutagen complements Tilt.** Mutagen moves files host→host and forwards
  ports; Tilt builds/deploys/watches inside the cluster on the target.
- Sync mode is `one-way-replica`, **including `.git`**, so a Mac `git checkout`
  propagates automatically. **Never run git on the target** — Mutagen will
  overwrite it.

## One-time Mutagen project setup

Templates live under [`hack/remote-sync/`](../hack/remote-sync/). Keep your
personal project **outside** the repo (paths differ per user/host).

Run from the CWE repo root (or adjust the `hack/remote-sync/...` paths). The
template uses a **+1000** local port offset (see below).

```bash
# Set once (shell rc is fine)
export REMOTE_SYNC_DIR=~/dev/remote-sync
export LOCAL_NKE_ROOT="$HOME/dev/nke"          # Mac absolute parent of the repos
export REMOTE_SSH_ALIAS=colossus               # SSH Host alias
export REMOTE_NKE_ROOT=dev/nke                 # path relative to remote $HOME

mkdir -p "$REMOTE_SYNC_DIR/$REMOTE_SSH_ALIAS"
sed \
  -e "s|__LOCAL_NKE_ROOT__|${LOCAL_NKE_ROOT}|g" \
  -e "s|__REMOTE_SSH_ALIAS__|${REMOTE_SSH_ALIAS}|g" \
  -e "s|__REMOTE_NKE_ROOT__|${REMOTE_NKE_ROOT}|g" \
  hack/remote-sync/mutagen.yml.example \
  > "$REMOTE_SYNC_DIR/$REMOTE_SSH_ALIAS/mutagen.yml"
cp hack/remote-sync/fetch-kubeconfig.sh "$REMOTE_SYNC_DIR/"

cd "$REMOTE_SYNC_DIR/$REMOTE_SSH_ALIAS"
mutagen project start
mutagen project list   # wait until sessions show "Watching for changes"
```

Useful commands (from the project directory):

```bash
mutagen project list
mutagen project flush      # force sync; also clears a wedged .git on the target
mutagen project terminate  # stop sync + forwards

# temporarily stop sync/resume of cloud-workflow-engine code changes - leaving the ports open
mutagen sync pause cloud-workflow-engine
mutagen sync resume cloud-workflow-engine
```

Run one Mutagen project at a time, or `terminate` before switching targets.

## Repos mirrored

Default template syncs:

- `cloud-workflow-engine`
- `cluster-api-provider-nico`
- `agent`
- `nke-site-deploy` (fleet `cwe-profiles` for `docker.local/cwe-profiles:tilt`)
- `forge-console` (external repo; CWE Tilt loads it as a provider)

Ignored on both sides (built/managed on the target): `bin/`, `var/`, `.dev/`,
`logs/`, `release/`, `images/`, `.gocache/`, `.gomodcache/`, `.cache/`,
`*.qcow2`, `*.log`, and similar.

## Port forwards (browse on the Mac)

Uniform per-target offset so remotes coexist with a local Mac Tilt stack (which
uses 3000 / 3100 / 8080 / 8081 / 18080 / 10350). Convention: **+1000 per
target**.

| target           | CWE   | TemporalUI | TemporalFE | Postgres | forge-vms | forge-console | Grafana | Tilt  |
| ---------------- | ----- | ---------- | ---------- | -------- | --------- | ------------- | ------- | ----- |
| local Mac (+0)   | 8080  | 8081       | 7233       | 5432     | 18080     | 8090          | 3000    | 10350 |
| colossus (+1000) | 9080  | 9081       | 8233       | 6432     | 19080     | 9090          | 4000    | 11350 |
| next (+2000)     | 10080 | 10081      | 9233       | 7432     | 20080     | 10090         | 5000    | 12350 |

The remote still binds the standard ports; only the Mac listener ports are
offset. Verify free ports with `lsof -nP -iTCP:<port> -sTCP:LISTEN` (note macOS
AirPlay may own 5000).

**colossus (+1000) URLs:**

- Tilt UI: http://localhost:11350
- CWE UI: http://localhost:9080 (Swagger: `/api/docs`)
- Temporal UI: http://localhost:9081
- forge-vms UI: http://localhost:19080/ui/
- forge-console UI: http://localhost:9090
- Grafana: http://localhost:4000
- Temporal frontend (gRPC / `temporal` CLI): `localhost:8233`
- Postgres: `localhost:6432`

Optional Loki / SPIRE forwards are commented in the template (`4100` / `31440`).

## kubectl to the remote kind cluster

The kind API server binds `127.0.0.1:<random-port>` on the target; its TLS cert
has a `127.0.0.1` SAN, so kubectl works over a Mutagen tunnel.

From `$REMOTE_SYNC_DIR` (sibling of the host project dir):

```bash
"$REMOTE_SYNC_DIR/fetch-kubeconfig.sh" --update-mutagen "$REMOTE_SSH_ALIAS"
# writes ~/.kube/<alias>-kind-cwe-capi.yaml, updates forward.kube-api in
# <alias>/mutagen.yml, restarts the Mutagen project

kubectl --kubeconfig "$HOME/.kube/${REMOTE_SSH_ALIAS}-kind-cwe-capi.yaml" get nodes
alias "k-${REMOTE_SSH_ALIAS}=kubectl --kubeconfig $HOME/.kube/${REMOTE_SSH_ALIAS}-kind-cwe-capi.yaml"
```

If you recreate the kind cluster, re-run the script — the remote API port is
dynamic.

## Daily loop on the target

SSH (or a remote terminal), then:

```bash
# recommended: keep tilt alive across SSH disconnect
tmux new -s tilt

cd ~/dev/nke/cloud-workflow-engine   # or whatever beta path you configured
export TILT_LINK_PORT_OFFSET=1000
tilt up
```
This starts:

- **forge-vms** — built with `make build-privileged`, live-reloaded on code changes
- **dnsmasq** — DHCP/DNS for VMs on the bridge (if installed on host)
- **Loki + Promtail + Grafana** — observability stack deployed into the Kind cluster
- **forge-vms-log-shipper** — host Promtail container that ships forge-vms and VM console logs to Loki
- **forge-vms-register-site** — registers the forge-vms site in CWE with the correct VIP CIDR

Reattach later with:
```bash
tmux attach -t tilt
```

## Running E2E tests on the remote

```bash
tmux new -s e2e

cd ~/dev/nke/cloud-workflow-engine

# Run any e2e desired e2e tests
make test-e2e
```

By running in a tmux session, the e2e tests will continue to run even if you disconnect from the SSH session.
You can also reconnect later to get the latest logs and results.

Reattach later with:
```bash
tmux attach -s e2e
```

I often just copy the logs from the test to my local host
```bash
ssh colossus 'tmux capture-pane -p -S -2000' | pbcopy
```

## Related docs

- [CWE README — CAPI + Tilt](../README.md#capi--tilt-development-recommended)
- [forge-vms README](../forge-vms/README.md)
- [forge-vms e2e testing (incl. one-shot remote rsync)](../forge-vms/docs/e2e-testing.md)
- [CWE E2E tests](../test/e2e/README.md)
- [`provision-colossus-local-forge` skill](https://gitlab-master.nvidia.com/nke/nke-agent-skills/-/tree/main/skills/provision-colossus-local-forge)
