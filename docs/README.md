# docs

| Document | Description |
|---|---|
| [architecture.md](architecture.md) | System overview — modules, data flows, deployment model |
| [getting-started.md](getting-started.md) | Local dev setup for controller and agent |
| [go-workspace.md](go-workspace.md) | The Go workspace: which modules are in it, and how to keep `go.work.sum` current |
| [branch-protection-settings.md](branch-protection-settings.md) | Admin guide: required GitHub branch protection rules |

Module-specific docs live alongside their code:

- `controller/docs/` — remote Colossus Tilt guide, and design proposals under
  discussion:
  [BYO-OS](../controller/docs/byo-os.md),
  [preprovisioned CAPI bootstrap](../controller/docs/preprovisioned-capi-bootstrap.md)
- `controller/AGENTS.md` — AI agent guide for CWE
- `agent/docs/` — agent design and security notes
- `agent/AGENTS.md` — AI agent guide for the node agent
