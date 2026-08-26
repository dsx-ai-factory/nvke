# The NKE API policy bundle

What `cedar-agent` loads beside NKE when `apiAuthz.enabled=true`. Four sources,
two of them generated, assembled into the three files the sidecar's flags name.

| File | Written by | Purpose |
|---|---|---|
| `policies.cedar` | hand | The rules. Native Cedar syntax, one `@id` per policy. |
| `roles.json` | hand | The roles, and which subject holds which. |
| `schema.json` | `hack/gen-authz-actions` | Entity and action types, from `api/openapi.yaml`. |
| `actions.json` | `hack/gen-authz-actions` | One entity per operation, each in its `read` or `write` group. |
| `bundle/` | `hack/authz-bundle` | The assembled result. Checked in, like `api/*.gen.go`. |

## Regenerating

```sh
go generate ./internal/authz/            # schema.json, actions.json, actions.gen.go
(cd hack/authz-bundle && GOWORK=off go run .)   # bundle/
```

The assembler is its own Go module so that `cedar-go` — which it uses only to
parse and split policy text — stays out of the controller's vendor tree. Hence
`GOWORK=off`, the same reason `api/generate.go` needs it.

## Why the assembly step exists at all

`cedar-agent` wants policies as a JSON array of `{id, content}` objects holding
**exactly one** Cedar statement each. A whole `.cedar` file handed to
`--policies` is rejected with `File is not a json file`; a whole file stuffed
into one `content` is rejected with `unexpected token permit`. Splitting on `;`
by hand would cut a policy in half at a semicolon inside a string literal, so the
assembler parses instead — which also means a syntax error fails the build rather
than becoming a sidecar with an empty policy store.

## Two things that will bite

**Load order is schema, then entities, then policies.** A schema loaded after a
policy that violates it is itself refused, leaving the store schema-less — and
with no schema, `cedar-agent` accepts a policy naming an action that does not
exist and it simply never matches. The chart's `args` are in this order
deliberately.

**A file that fails to load is not fatal to the sidecar.** It logs an `ERROR`,
starts anyway with that store empty, and answers `200 OK` — `Deny` to everything
if the entities did not load, which at request time is indistinguishable from a
policy nobody wrote. NKE checks the stores itself before it serves; see
`internal/authz.CedarAgent.Ready`. When a change here produces blanket 403s, read
the sidecar's log first.

## Action groups, and what they cannot express

`read` and `write` are derived from the HTTP method, so every `GET` is a `read`
and everything else is a `write`. That is what lets `permit(principal in
NKE::Role::"reader", action in NKE::Action::"read", resource)` stay correct as
endpoints are added.

It also means an operation cannot be kept *out* of its group by a `permit`.
`deleteNode` is a `DELETE`, so `operators-may-write` would grant it; holding it
to one role takes a `forbid ... unless`, which beats every `permit`. Anything
irreversible wants that treatment — see `only-node-operators-delete-nodes`.

## Testing a change

```sh
docker run --rm -d --name nke-cedar -p 8190:8180 \
  -v "$PWD/deploy/authz/bundle:/bundle:ro" permitio/cedar-agent:0.2.2 \
  --schema /bundle/schema.json --data /bundle/entities.json --policies /bundle/policies.json
AUTHZ_TEST_ENDPOINT=http://localhost:8190 go test -tags authz_live ./internal/authz/ -v
```

`TestLiveDecisionMatrix` asserts what this bundle grants. A policy change that
alters who may do what should change that table in the same commit.
