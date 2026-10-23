# Configuring NVKE API Authorization

How to decide what each authenticated caller may do on the NVKE API, by granting
roles.

This is the operator's side, and it comes after authentication. For turning
authentication on, see
[Configuring NVKE API Authentication](configuring-api-authentication.md).

## What you are setting up

Authentication decides who a caller is. Authorization decides what that caller
may do. It has the same states:

| State | Behaviour |
|---|---|
| **Off** | Every caller authentication admits can call every endpoint. This is the default. |
| **Permissive** | Every request is checked against the policy and counted, then served anyway. |
| **Enforcing** | A request no policy permits gets a 403. |

The chart ships the roles and the policies that give each role its permissions:

| Role | Holds | Can |
|---|---|---|
| `reader` | | Call every read operation. |
| `operator` | `reader` | Make any change except `deleteNode`, `deleteNodePool` and `updateNodePool`. |
| `nodeOperator` | `reader` | Call `deleteNode`, `deleteNodePool` and `updateNodePool`, and make no other change. |
| `admin` | `operator`, `nodeOperator` | Everything. |

You write one file, `roleMappings.json`, which says who holds which role. It
lives in a ConfigMap you create. The chart mounts it and never renders or
overwrites it.

A caller holds no role until a mapping grants one. Under the shipped policies,
a caller with no role gets 403 on every endpoint once enforcing.

## Before you start

**Authentication is on.** The chart refuses to render `apiAuthz.enabled: true`
without `apiAuth.enabled: true`. Take authentication to enforcing first. While
it is permissive, a request it would have rejected reaches authorization with no
caller, and authorization refuses it with reason `no_principal`.

You need permission to set Helm values on the NVKE release and to create a
ConfigMap in its namespace. The chart version needs an `apiAuthz` section:

```bash
CHART=...   # your NVKE chart reference
helm show values "$CHART" | grep -A12 '^apiAuthz'
```

The commands below use `$NS` for the namespace NVKE runs in:

```bash
NS=nke-controller   # your release namespace
```

## Turning it on

### 1. Write `roleMappings.json`

A JSON array, one entry per mapping:

```json
[
  {
    "issuer": "local-kubernetes",
    "serviceAccount": { "namespace": "nke-clients", "name": "nke-client" },
    "grants": [{ "type": "NKE::Role", "id": "admin" }]
  },
  {
    "issuer": "entra",
    "claim": { "name": "groups", "value": "<the group's object ID>" },
    "grants": [{ "type": "NKE::Role", "id": "operator" }]
  },
  {
    "issuer": "entra",
    "claim": { "name": "oid", "value": "<the user's object ID>" },
    "grants": [{ "type": "NKE::Role", "id": "admin" }]
  },
  {
    "issuer": "keycloak",
    "claim": { "name": "sub", "value": "<the user's ID in Keycloak>" },
    "grants": [{ "type": "NKE::Role", "id": "reader" }]
  },
  {
    "issuer": "spire",
    "spiffe": { "trustDomain": "example.org", "expression": "path == '/nke-service/node-lifecycle'" },
    "grants": [{ "type": "NKE::Role", "id": "nodeOperator" }]
  }
]
```

In order, these grant:

1. `admin` to `nke-clients/nke-client`, the service account `nvkectl` mints for.
2. `operator` to everyone in an Entra group.
3. `admin` to one Entra user.
4. `reader` to one Keycloak user.
5. `nodeOperator` to one SPIFFE workload.

**`issuer`** is the `name` of an issuer in your authentication config, such as
`local-kubernetes` or `spire` from its recipes. A mapping matches only tokens
from the issuer it names.

**`grants`** lists the roles a matching caller holds. A caller holds the roles
of every mapping that matches it.

Each mapping sets exactly one of:

- **`serviceAccount`**, a Kubernetes namespace and service account. Omit `name`
  to match every service account in the namespace.
- **`claim`**, a claim in the verified token. `name` is the claim and `value` is
  what it must hold. The claim can be a string or an array of strings, such as
  `groups`.
- **`spiffe`**, a trust domain and a CEL expression over the SPIFFE ID's path.
  The expression sees one variable, `path`, and returns `true` to match.

**`nvkectl` uses one account for everyone.** It always mints for
`nke-clients/nke-client`, so the role you grant that account goes to everyone
who can mint a token for it. That is whoever holds `create` on
`serviceaccounts/token` for it; see
[step 2 of the authentication guide](configuring-api-authentication.md#2-create-the-identity-your-callers-will-use).

**Grant one person by a claim that identifies them.** Use `sub` for most
issuers. Use `oid` for Entra, whose `sub` differs for every application. Entra
puts group object IDs in `groups`, not display names.

**Match only claims the issuer asserts about the caller.** Never map on a claim
the client chooses, such as `nonce` or `scope`. `aud` is refused at load for
that reason.

A mapping grants a role only to a caller authentication already admits. It
never admits anyone.

### 2. Create the ConfigMap

```bash
kubectl -n "$NS" create configmap nke-authz-role-mappings \
  --from-file=roleMappings.json=roleMappings.json \
  --dry-run=client -o yaml | kubectl apply -f -
```

The key must be `roleMappings.json`. Create the ConfigMap before you turn
authorization on: NVKE does not start without it.

To use another name, set `apiAuthz.roleMappingsConfigMap` to it. The ConfigMap
must be in the release namespace.

### 3. Set the chart values

```yaml
apiAuthz:
  enabled: true
  permissive: true
```

Start permissive. The next section covers going enforcing.
`apiAuthz.permissive` is separate from `apiAuth.permissive`.

### 4. Check it came up

```bash
kubectl -n "$NS" logs deploy/nke-controller | grep 'API authorization'
```

```json
{"msg":"API authorization is enabled","mode":"permissive","engine":"cedar-go","source":"/etc/nke/authz"}
```

If NVKE does not start, see [Troubleshooting](#troubleshooting).

## Going enforcing without locking callers out

Do not skip permissive. It shows every caller your mappings miss before any of
them is refused.

Watch the counter:

```bash
kubectl -n "$NS" exec deploy/nke-controller -- \
  curl -s http://127.0.0.1:9090/metrics | grep '^nke_authz_requests_total'
```

```
nke_authz_requests_total{action="listClusters",outcome="allowed",reason=""} 42
nke_authz_requests_total{action="createCluster",outcome="would_reject",reason="no_policy_match"} 3
```

Every `would_reject` series is a request enforcing would refuse. The log names
the caller behind each one:

```bash
kubectl -n "$NS" logs deploy/nke-controller | \
  grep 'authorization would have refused this request'
```

Each line carries `subject`, `issuer` and `action`. For a caller that should
have access, add a mapping and wait for the reload (see
[Changing and revoking grants](#changing-and-revoking-grants)). For one that
should not, leave it.

When `would_reject` stops climbing for callers you mean to keep, set
`apiAuthz.permissive: false`.

## Changing and revoking grants

Edit `roleMappings.json` and apply it with the same command as step 2. NVKE picks
up the change without a restart:

1. The kubelet updates the mounted file. This can take up to its sync period,
   one minute by default.
2. NVKE re-reads and re-validates the file every `AUTHZ_RELOAD_INTERVAL`, 30
   seconds by default.

Confirm the change is live:

```bash
kubectl -n "$NS" logs deploy/nke-controller | grep 'authorization bundle reload'
```

```json
{"msg":"authorization bundle reloaded","source":"poll","dir":"/etc/nke/authz","changed":true}
```

**A bad edit changes nothing.** NVKE logs `authorization bundle reload failed`
with the error, counts `nke_authz_reload_total{outcome="failed"}`, and keeps
the previous grants live. Fix the file and apply it again.

**To revoke,** delete the mapping, or remove the role from its `grants`. The
next request after the reload is decided without it. A caller does not need a
new token.

**Removing the last mapping revokes nothing.** NVKE refuses a `[]` file on
reload, as at startup, and the previous grants stay live. Keep at least one
mapping.

**A group change in your identity provider** reaches NVKE only in the caller's
next token.

**A mapping for a new issuer** loads only after NVKE restarts with that issuer in
its authentication config.

To reload without waiting for the poll, a caller holding `operator` can
call `POST /api/authz/reload`. It needs authentication and authorization both
enforcing, and reloads only the replica that answers.

## Reference

### Chart values

| Value | Default | Notes |
|---|---|---|
| `apiAuthz.enabled` | `false` | Needs `apiAuth.enabled`. |
| `apiAuthz.permissive` | `false` | |
| `apiAuthz.roleMappingsConfigMap` | empty | The ConfigMap holding `roleMappings.json`. Empty means `nke-authz-role-mappings`. |
| `apiAuthz.existingConfigMap` | empty | Your own `policies.cedar` and `roles.json`. Empty runs the shipped ones. See below. |

The chart refuses to render either ConfigMap value if it is not a string, or
if it names the chart's own `<release fullname>-authz-default`.

`AUTHZ_RELOAD_INTERVAL` sets the reload period. Set it through the chart's
`env` list.

### Mapping fields

| Field | Required | Notes |
|---|---|---|
| `issuer` | yes | An `issuers[].name` from the authentication config. |
| `grants` | yes | At least one `{ "type": "NKE::Role", "id": ... }`. Every role must exist. |
| `serviceAccount.namespace` | with `serviceAccount` | |
| `serviceAccount.name` | no | Omitted, matches every service account in the namespace. |
| `claim.name` | with `claim` | A top-level claim. Not `aud`. |
| `claim.value` | with `claim` | Not empty. Matches the claim, or one element of an array of strings. An array holding a non-string matches nothing. |
| `spiffe.trustDomain` | with `spiffe` | A SPIFFE ID outside it never matches. |
| `spiffe.expression` | with `spiffe` | CEL over `path`, returning a bool. `split()` is available. |

Values are matched exactly. Leading or trailing whitespace in `issuer` or
`claim.name` is an error.

A SPIFFE expression that errors does not match. One evaluation can spend 10,000
CEL cost units, and one request's `spiffe` mappings 100,000 between them. A
mapping past either limit does not match.

### Why a request was refused

Every refusal is a 403. The response names the reason in words; the log and the
`reason` label carry the value below.

| `reason` | Meaning |
|---|---|
| `no_policy_match` | No policy permits this caller the action, or a `forbid` applies. Usually a missing mapping. |
| `no_principal` | Permissive authentication let through a caller it did not admit. |
| `no_route` | No endpoint serves this path. |
| `unknown_operation` | NVKE serves the endpoint but has no action for it. NVKE's problem, not the caller's. |
| `ambiguous_path` | The path reads differently escaped and decoded. |
| `invalid_query` | The request could not be put to the policy. |
| `engine_error` | The policy engine failed. NVKE's problem, not the caller's. |

Authorization skips the same endpoints authentication does; see
[Endpoints that need no token](configuring-api-authentication.md#endpoints-that-need-no-token).

### Metrics

| Metric | Labels | Meaning |
|---|---|---|
| `nke_authz_requests_total` | `outcome`, `reason`, `action` | Decisions. `outcome` is `allowed`, `would_reject` or `rejected`. |
| `nke_authz_enabled` | `mode` | 1 for the configured mode: `off`, `permissive` or `enforcing`. |
| `nke_authz_reload_total` | `outcome`, `source` | Reloads. `outcome` is `succeeded`, `failed` or `throttled`. |
| `nke_authz_bundle_loaded_timestamp_seconds` | | When a reload last changed what NVKE enforces. |

### Your own policies and roles

Set `apiAuthz.existingConfigMap` to a ConfigMap holding your `policies.cedar`
and `roles.json`. NVKE still takes `roleMappings.json` from the mappings
ConfigMap and `actions.json` from the chart. A missing key holds the pod in
`ContainerCreating`. Start from the files in
[`deploy/authz/`](../deploy/authz/README.md) at the release you deploy.

Grant roles in `roleMappings.json`, not in a policy. A policy that names a
caller, such as `principal == NKE::Principal::"user-1"`, matches that subject
from every issuer.

## Troubleshooting

**NVKE does not start, and the error says `reading roleMappings.json`.** The
ConfigMap is missing, is in another namespace, or has no `roleMappings.json`
key:

```
reading roleMappings.json: open …/roleMappings.json: no such file or directory; the chart mounts it from the ConfigMap named by the chart value apiAuthz.roleMappingsConfigMap (nke-authz-role-mappings when empty), which must exist and hold a roleMappings.json key
```

Check the name against `apiAuthz.roleMappingsConfigMap`:

```bash
kubectl -n "$NS" get configmap nke-authz-role-mappings -o jsonpath='{.data.roleMappings\.json}'
```

**NVKE does not start, and the error says `roleMappings.json has no entries`.**
The file holds `[]`. Add a mapping.

**NVKE does not start, and the error says `roleMappings.json names issuer(s)`.**
A mapping's `issuer` matches no `issuers[].name` in the authentication config.
Fix the mapping, or add the issuer to the authentication config:

```
roleMappings.json names issuer(s) ["entra"], which the authentication config does not configure (configured: ["local-kubernetes"]); fix the name or add the issuer to AuthenticationConfiguration
```

**NVKE does not start, and the error names `roleMappings.json[N]`.** Entry `N`,
counting from 0, is malformed. Usual causes: a role in `grants` that does not
exist, more or fewer than one of `claim`, `serviceAccount` and `spiffe`, an
empty `claim.value`, or `claim.name` set to `aud`.

**NVKE does not start, and the error says `the field is "grants"`.** An entry
uses `parents`, the `roles.json` key, where a mapping uses `grants`:

```
parsing roleMappings.json: unknown field "[0].parents"; the field is "grants"
```

**NVKE does not start, and the error says a file `declares NKE::Principal`.**
Your `roles.json` declares a caller. Remove it and grant its roles in
`roleMappings.json`:

```
roles.json declares NKE::Principal::"user-1", which would hold its roles for that subject from every issuer; remove it and grant its roles with a roleMappings.json entry naming the caller's issuer
```

Each of these errors is in the log under `Failed to set up API authorization`:

```bash
kubectl -n "$NS" logs deploy/nke-controller | grep 'Failed to set up API authorization'
```

**The install fails with `apiAuthz.enabled requires apiAuth.enabled`.** Turn
authentication on first.

**A caller gets 403 with `Not authorized: no policy permits this action.`**
Its token is valid and no mapping grants it a role that allows the action. The
log line names the `subject` and `issuer`. Check that a mapping names that
issuer and matches that caller. For a `claim` mapping, decode the token and
check it carries the claim. Re-minting helps only after a change in the
identity provider.

**An edit has no effect.** Look for `authorization bundle reload failed` in the
log. A failed reload keeps the previous grants. Otherwise, allow for the kubelet
sync period plus `AUTHZ_RELOAD_INTERVAL`.

**You cannot tell whether authorization is on.** Read the metric:

```bash
kubectl -n "$NS" exec deploy/nke-controller -- \
  curl -s http://127.0.0.1:9090/metrics | grep '^nke_authz_enabled'
```

## See also

- [Configuring NVKE API Authentication](configuring-api-authentication.md): turning on authentication, and the identities `nvkectl` uses.
- [Authenticating to the NVKE API](authenticating-to-the-api.md): the caller's side.
- [`deploy/authz/README.md`](../deploy/authz/README.md): the shipped policy bundle.
