# AGENTS.md

## Scope

These instructions apply to the `cli/` Go module and its end-to-end test
fixtures.

## Verification

Run these commands from `cli/`:

```bash
make fmt
make lint
make test
```

Run `make preflight` from the repository root before submission. The bootstrap
end-to-end test also requires Docker, Kind, `/dev/vtpmx`, and the CWE image.

After a dependency change, run `go mod tidy` in `cli/` and `make work-sync` at
the repository root.

## Go documentation comments

- Start each exported declaration comment with the declaration name.
- State the API contract. Explain what the function accepts and returns, or
  what side effect it performs.
- Use established domain terms, such as "out-of-band provisioning" and
  "initial handshake."
- Do not put design rationale, implementation history, pull-request context,
  or a detailed implementation walkthrough in a function comment.
- Put necessary design rationale in a design document. Link to that document
  from the comment only when callers need the context to use the API correctly.
- Put short implementation comments next to non-obvious code. Explain why the
  code is necessary, not what the syntax does.

Follow the Go documentation comment guidance at
<https://go.dev/doc/comment#func>.
