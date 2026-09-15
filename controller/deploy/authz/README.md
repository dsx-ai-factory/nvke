# The NKE API policy bundle

What NKE evaluates when `apiAuthz.enabled=true`. One ConfigMap, mounted read-only
at `/etc/nke/authz`, read at startup by `cedar-go` inside the NKE container.

| File | Written by | Purpose |
|---|---|---|
| `policies.cedar` | hand | The rules. Native Cedar, one `@id` per policy. |
| `roles.json` | hand | The roles, and which subject holds which. |
| `actions.json` | `hack/gen-authz-actions` | One entity per API operation, each in its `read` or `write` group. |
| `schema.json` | `hack/gen-authz-actions` | The entity and action types. **Not read at runtime** — see below. |

## Regenerating

```sh
go generate ./internal/authz/     # actions.gen.go, actions.json, schema.json
```

`policies.cedar` and `roles.json` are yours. The other two are derived from
`api/openapi.yaml` and regenerating is not optional: `actions_test.go` fails if
the table and the spec disagree.

## What the schema is for

`schema.json` is never loaded by the running process. It exists so that CI can
type-check the bundle: `policy_validation_test.go` feeds it to cedar-go's
validator and rejects a policy that names an action or entity type that does not
exist, reads an undeclared attribute, applies an action to the wrong resource
type, or compares a value against the wrong type. It also checks that every one
of the 22 operations produces a request the schema accepts.

That runs in tests only, deliberately: the validator lives under `x/exp` and a
build-time check gets the benefit without the release binary depending on
experimental API. The cost is that a bundle hand-edited in a cluster is not
re-checked — it will parse, and a policy that can never match will simply never
match.

Schema validation is type-based, so it cannot tell you a role *instance* does not
exist: `NKE::Role::"raeder"` is a well-typed reference to a role nobody declared.
`policy_vocabulary_test.go` covers that half.

## Policies may not read entity attributes

`principal`, `action` and `resource` carry no attributes, and `NewLocal` refuses
a policy that reads one — in the cluster, not only in CI.

Nothing populates them and nothing can here. The resource is synthesized from the
request path before the handler runs, so this process has not read a database and
does not know what site a cluster is in. A rule like

```cedar
forbid(principal, action in NKE::Action::"write", resource)
when { resource has site && resource.site == "prod" };
```

reads as a restriction, type-checks, loads, and applies to nothing — `resource
has site` is false for every request, so the `forbid` is skipped and the `permit`
it was written to override wins. That is the one policy mistake here that fails
*open*, which is why it is refused at load rather than left to a test.

`context` is the exception: `context.issuer` and `context.method` are populated
per request and declared in the schema.

## Action groups, and what they cannot express

`read` and `write` are derived from the HTTP method: the safe methods (`GET`,
`HEAD`, `OPTIONS`, `TRACE`) are a `read`, everything else a `write`. That is what
lets `permit(principal in NKE::Role::"reader", action in NKE::Action::"read",
resource)` stay correct as endpoints are added.

It also means an operation cannot be kept *out* of its group by a `permit`.
`deleteNode` is a `DELETE`, so `operators-may-write` would grant it; holding it
to one role takes a `forbid ... unless`, which beats every `permit`. See
`only-node-operators-change-node-lifecycle`.

The method says nothing about consequence, so that treatment is not optional and
not left to judgement: `policy_fences_test.go` requires every write operation to
be named by a policy or listed in `writesLeftToTheGroup` with a reason. Adding a
destructive endpoint without doing one of the two fails the build, which is the
diff a reviewer needs and would not otherwise get.

There is a limit worth knowing. One `operationId` is one Cedar action, and this
gate decides before the request body is parsed — so `updateCluster`, which
carries every cluster-level PATCH field (control-plane node recycling, a
Kubernetes version upgrade, Teleport enrollment, nested control-plane config,
and the one-way `enableEtcdEncryption`/`enableOidc`) in one request, cannot be
split by any rule here. Separating those is an API change.

See `writesLeftToTheGroup` and the `exception` type in `policy_fences_test.go`
for the full argument, blast radius, owner, and re-evaluation trigger behind
each operation left to the coarse grant.

## Testing a change

```sh
go test ./internal/authz/
```

`TestLocalMatchesTheBundle` asserts what this bundle grants, case by case. A
policy change that alters who may do what should change that table in the same
commit — if it does not, either the change did nothing or the table was not
covering it.

## History

An out-of-process engine — a `permitio/cedar-agent` sidecar — was built against
this same bundle and dropped. It needed the sources assembled into a different
shape, which is why `hack/authz-bundle` and `deploy/authz/bundle/` existed. The
design's Appendix C records why it went; the branch
`prototype/api-authz-cedar-sidecar` has the working code if it is ever wanted.
