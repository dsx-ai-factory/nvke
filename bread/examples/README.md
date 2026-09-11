# examples/

Example manifests for the five PoC kinds.  Every manifest shows
every field of its kind's API; the comments say which are live, which are shape and which are later.

| Directory | What | Who applies it, where |
|---|---|---|
| `examples/*.yaml` | The happy path: Release, ReleaseChannel, NKECluster demo, NodePools gpu-a and gpu-fixed | `kubectl apply -f examples/` (see below) |
| `examples/promote/` | The promotion story, applied one file at a time in number order: a second Release, the channel promoted, the cluster pinned (hold), the old Release deprecated, then hidden | operator files on the internal group; `31-nkecluster-pin.yaml` as the tenant |
| `examples/invalid/` | One rejected write per file; the header states the rule and the expected error class | as the tenant; each is refused, nothing is stored |
| `examples/reference/` | A full NKENode as the controller creates it. Not applyable: `kubectl get nkenodes -n org-demo` to see real ones | nobody |
| `examples/org/` | The Org fixture that makes org-demo exist; applied before the API server starts | operator, management cluster |

## Order

Read them in this order; it is also the order they are applied once the API server exists (that,
and the tooling to apply them, arrive in later PRs — see `internal/apis/README.md`).

1. `10-release.yaml`, `20-release-channel.yaml`: the operator catalog, written on the management
   cluster's internal group.
2. `30-nkecluster.yaml`, then `40-nodepool.yaml` and `41-nodepool-fixed.yaml`: tenant objects,
   written as org-demo's admin through nke-apiserver.
3. `promote/` in number order, then `invalid/` one file at a time to see each rule.

## Legend

- Plain field: populated and acted on in the PoC.
- `# shape: <meaning>; future writer: <who>`: in the API, accepted, validated and round-tripped,
  but no PoC component writes or acts on it. Tenant kinds and P0 requirements only. Its kubectl
  column renders blank.
- `# later (<SRD ref>): <field>: <value>`: recorded so it is not designed twice; not in the API and
  refused by the strict decoder if sent.
- `# status:` blocks are written by the controller and shown for reference; a tenant body that
  carries status is refused.
