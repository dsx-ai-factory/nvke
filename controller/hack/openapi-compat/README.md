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
`github.com/dsx-ai-factory/nvke/controller/api@v0.0.17`. That pin is exactly what
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
   The exception is a waiver that records the `baseline` it was written
   against: once the tag being compared against is a *later release*, a waiver
   that matches nothing is *expired*, not stale. Its break has shipped in a
   newer tag, so it is reported and the gate still passes, rather than failing
   every branch that rebases past the tag. Only that one relation expires a
   waiver, because `--baseline` is caller-supplied: a waiver with no
   `baseline`, one that matches nothing against the baseline it names, one
   checked against an *older* release, and one whose `baseline` is not a
   `controller/api/vX.Y.Z` release tag all still fail.

The failure message prints a pasteable waiver stub for each unwaived change.

### Removal has a second route

A waiver is not the only way to remove an operation, and it should not be the
usual one. An operation that was marked `deprecated: true`, carried an
`x-sunset` date, and whose date has passed may be removed with **no waiver at
all**: the gate reports the removal as one it did not have to be asked about.

That route exists because without it the patient path and the abrupt one cost a
reviewer exactly the same — one waiver either way — and the gate would be
quietly arguing against the behaviour it is supposed to encourage.

Its price is that the deprecation has to be a real one, which `lifecycle.go`
enforces. Each of these fails:

| Declaration | Why it is not a deprecation |
|---|---|
| `deprecated: true` with no `x-sunset` | Open-ended. Nothing a consumer can plan around. |
| `x-sunset` that is not a `YYYY-MM-DD` date | Cannot be honoured, so the removal rule could never fire. |
| `x-sunset` less than `deprecationNoticeDays` away | A fortnight is not notice. |
| `x-sunset` on an operation not marked `deprecated` | `deprecated` is the half that reaches a generated client and the rendered reference. The date alone tells nobody. |

Every one of those rules is scoped to what **changed**. An operation whose
deprecation fields are identical in both specifications is not examined at all,
however they are set. A state rule would fail a pull request that never touched
the specification the moment some older deprecation drifted out of compliance,
and a gate that fails unrelated work is a gate someone turns off.

The route covers operations only. The engine reports a removed operation as a
single change naming it — `object_removed` at `$.paths` for a whole path item,
`property_removed` at `$.paths['<path>']` for one method — which is what makes
"this exact removal was announced" decidable. A removed property or parameter is
reported in a shape where it is not, so those are breaks like any other and take
a waiver.

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
| Either specification cannot be read as `paths` | The deprecation rules would then see no operations, and every removal would look un-announced rather than un-checked. |

The "no changes" case is the one positive result the engine reports with no
`changes` array, and it is matched explicitly. A future rewording of that message
fails the gate rather than being mistaken for success.

## Testing it

```bash
go test ./hack/openapi-compat/
```

The policy, the deprecation rules, the waiver schema and the report parser are
unit tested, including every fail-closed case above. The deprecation tests fix
the clock, so a sunset date cannot start passing or failing with the calendar.
The tests do not invoke the engine or the network.

To see it reject something, break the specification on purpose — change a
response code, remove a required property — and run the target.

To see the deprecation route end to end, which needs a baseline that already
carries the deprecation: commit a specification with `deprecated: true` and a
past `x-sunset` on one operation, then remove that operation and compare against
the commit you just made.

```bash
make -C controller api-compat-check API_COMPAT_BASELINE=<that commit>
```

[engine]: https://github.com/pb33f/openapi-changes
