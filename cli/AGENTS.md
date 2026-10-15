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
end-to-end test also requires nerdctl with containerd, a buildkitd for
`nerdctl build` to reach, Kind, `/dev/vtpmx`, and the CWE image. It does not
need Docker. It does need root — it installs an iptables rule and assigns an
address to a bridge — so run it under `sudo`.

After a dependency change, run `go mod tidy` in `cli/` and `make work-sync` at
the repository root.

## The command reference

`docs/reference/` is generated from the command tree, one page per public
command. After any change to a command — a new command or subcommand, a
renamed flag, a changed default, edited help text, an added alias, a
deprecation — run:

```bash
make -C cli docs
```

and commit the result with the change that caused it. `make -C cli docs-check`
verifies it, and the merge gate fails when the committed pages do not match the
tree.

Do not edit the generated pages. A page says what the command's own metadata
says, so change the `Short`, `Long`, `Example`, `Args`, `Aliases`,
`Deprecated` or flag usage string on the command itself.

A constraint enforced only inside `RunE` is invisible to cobra, so it never
reaches the reference on its own. State it in the command's `Long` text or in
the flag's usage string, the way `--nodes` and `--sitespec` already say
`(required)`.

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
