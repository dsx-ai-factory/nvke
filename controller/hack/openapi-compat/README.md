# openapi-compat

Fails when a change to `controller/api/openapi.yaml` would break an existing
client.

```bash
make -C controller api-compat-check
```

## Why this exists separately from generate-check

`make -C controller generate-check` proves the committed `*.gen.go` match the
specification. That is regeneration *correctness*, and it is a different
question from *compatibility*.

A removed field, a narrowed type or a newly required property passes
`generate-check` cleanly — the generated code matches the new specification
perfectly — while breaking every client built against the old one. Nothing asked
whether the specification change was safe in the first place. This is that
layer.

## The baseline

The newest tag matching `controller/api/v[0-9]*.[0-9]*.[0-9]*`.

Those tags are published by
[`controller-api-tag.yml`](../../../.github/workflows/controller-api-tag.yml) on every
`main` commit that touches `controller/api`, so that a consumer can pin
`github.com/dsx-ai-factory/nke/controller/api@v0.0.17`. That pin is exactly what
"backward compatible" has to be measured against — it is the artefact somebody
is holding. The merge base would be a weaker baseline: it is a commit nobody
depends on, and it drifts every time `main` moves.

Pass `--baseline <ref>` to compare against something else. `make
api-compat-check API_COMPAT_BASELINE=controller/api/v0.0.12` is the usual form,
for checking a change against an older release rather than the latest.

## The policy

1. The change engine ([`openapi-changes`][engine], pinned as
   `OPENAPI_CHANGES_VERSION` in `make/tool-versions.mk`) classifies every
   difference as breaking or additive. That classification is the engine's and
   is not reimplemented here; a second opinion about what OpenAPI means would
   only diverge from the first.
2. Every breaking change fails the gate.
3. Unless `controller/api/openapi-compat-waivers.yaml` carries a waiver that
   matches it exactly, on `change`, `path` and `property`. A waiver must name a
   reason, a person, and a review date.
4. A waiver that matches nothing also fails, so an entry cannot outlive the
   change it was written for. This is the same rule `GOVULNCHECK_WAIVED` is held
   to.

The failure message prints a pasteable waiver stub for each unwaived change.

Waivers are matched on `change` + `path` + `property`, deliberately not on the
engine's `changeHash`. That hash covers the *shape* of a change and not its
location: in one sample diff, 89 of 91 breaking changes shared five hashes. A
waiver keyed on it would silently cover every identically shaped change anywhere
in the document.

The engine also accepts a `changes-rules.yaml` that reclassifies whole
categories (`--config`). It is not used. Turning off "removal from components is
breaking" across the board would be a much larger, much quieter decision than
waiving one change with a name against it.

## It fails closed

A compatibility gate that could not run has not found anything compatible. Each
of these is an error, never a pass:

| Condition | Why it is not a pass |
|---|---|
| No tag matches the pattern | A shallow CI checkout has no tags. There is no baseline, so nothing was compared. |
| The baseline file is missing or empty at that ref | Same: nothing was compared. |
| Either specification will not parse | The engine exits non-zero and writes nothing to stdout. Reading that as zero breaking changes is the exact failure this gate exists to prevent. |
| The engine's report is empty, truncated, or an unrecognised shape | The output format has moved. Fix the tool; do not assume. |
| The waiver file is missing | It is committed. Its absence means the path or the checkout is wrong — not that nothing is waived. |
| A waiver has an unknown key | A misspelled key would make the waiver a silent no-op. |

The "no changes" case is the one positive result the engine reports with no
`changes` array, and it is matched explicitly. A future rewording of that message
fails the gate rather than being mistaken for success.

## Testing it

```bash
go test ./hack/openapi-compat/
```

The policy, the waiver schema and the report parser are unit tested, including
every fail-closed case above. The tests do not invoke the engine or the network.

To see it reject something, break the specification on purpose — change a
response code, remove a required property — and run the target.

[engine]: https://github.com/pb33f/openapi-changes
