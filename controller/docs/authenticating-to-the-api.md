# Authenticating to the NVKE API

Your local cluster can issue you a JWT, and NVKE will accept it. No identity
provider, no SPIRE, no extra infrastructure. The Kubernetes API server you
already have is a token issuer, and NVKE is configured to trust it.

This walks through getting a token and using it, and what to do when you get a
401 you weren't expecting. For the configuration schema, RBAC and the full list
of rejection reasons, see [API Authentication](../README.md#api-authentication)
in the main README.

## The short version

With `nvkectl`, point at the cluster Tilt created and call the API. The CLI
mints for `nke-clients/nke-client`; you do not export a token.

```bash
nvkectl config set-site --name local \
  --url http://localhost:8080 \
  --kubeconfig ~/.kube/config
nvkectl get clusters
```

With `curl`, mint a token yourself:

```bash
TOKEN="$(hack/nvke-token.sh)"
curl -H "Authorization: Bearer ${TOKEN}" http://localhost:8080/api/clusters
```

If that returns your clusters, you're done. The rest of this page explains what
the token path did, so you can fix it when it doesn't.

## Before you start

You need a kind cluster running NVKE:

```bash
tilt up
```

That deploys an enforcing NVKE. Tilt forwards the API to `localhost:8080` and
applies `deploy/k8s/nvke-auth-dev.yaml`, which creates the client identities used
below.

`NKE_AUTH_ENABLED=false tilt up` turns authentication off, and then none of this
is necessary.

## Is authentication even on?

Check this first: "it works without a token" and "my token is being
ignored" look identical from the outside.

```bash
curl -s -o /dev/null -w '%{http_code}\n' http://localhost:8080/api/clusters
```

`200` means authentication is off: NVKE serves everyone, and nothing below is
necessary. `401` means it's on.

If you want to see it from the deployment rather than infer it:

Tilt runs NVKE as Deployment `nvke-controller` in namespace `nvke-controller`. On a
site that still runs it under an older name, use that name for both:
`nke-controller` for a site installed before the `nvke` rename,
`cloud-workflow-engine` for anything older.

```bash
kubectl -n nvke-controller get deploy nvke-controller \
  -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="AUTH_CONFIG_PATH")].value}{"\n"}'
```

Empty means off. NVKE also says so at startup:

```json
{"level":"INFO","msg":"API authentication is enabled","mode":"enforcing",
 "config_path":"/etc/nke/auth/auth.yaml","audiences":["https://nke.tilt.local"],
 "issuers":["local-kubernetes"]}
```

## Step 1: find the audience

A token has to say who it's for. NVKE rejects any token whose `aud` claim doesn't
name it, and that rejection is a 401 that looks exactly like sending no token at
all. Getting the audience right is the single most common thing to get wrong.

Locally it's `https://nke.tilt.local`, but read it rather than assume it:

```bash
hack/nvke-token.sh --audience
```

Or from the ConfigMap directly:

```bash
kubectl -n nvke-controller get configmap nvke-auth-config \
  -o jsonpath='{.data.auth\.yaml}' | grep audiences
```

```yaml
audiences: ["https://nke.tilt.local"]
```

This value is per-install. On a real management cluster it is different.

## Step 2: pick an identity

NVKE doesn't accept every token the cluster signs. If it did, anyone who can
create a pod could get administrative access: the kubelet will project a token
for their service account naming any audience they ask for, and no RBAC check
applies to that. So NVKE keeps an allow-list of admitted subjects, and everyone
else is refused even with a perfectly valid token.

`deploy/k8s/nvke-auth-dev.yaml` sets up two identities for local work:

| Identity | Admitted? | What you get |
| --- | --- | --- |
| `nke-clients/nke-client` | yes | `200` |
| `nke-outsiders/nke-outsider` | no | `403` |

Use `nke-client`. The outsider exists so you can prove the rejection path on
purpose; see [Proving a rejection](#proving-a-rejection).

## Step 3: mint the token

```bash
kubectl create token nke-client -n nke-clients \
  --audience=https://nke.tilt.local \
  --duration=10m
```

That prints a JWT to stdout.

It has to be `kubectl create token`. That goes through the TokenRequest API,
which produces a token bound to the audience you asked for. The old approach,
a ServiceAccount `Secret`, has no audience at all, so NVKE refuses it.

`--duration` is a request, not a guarantee. The API server clamps it to its
own configured maximum. Ask for 10 minutes while you're poking at things; ask for
longer and you'll be re-minting less often but leaving a longer-lived credential
lying around in your shell history.

## Step 4: call the API

```bash
TOKEN="$(kubectl create token nke-client -n nke-clients \
  --audience=https://nke.tilt.local --duration=10m)"

curl -H "Authorization: Bearer ${TOKEN}" http://localhost:8080/api/clusters
```

`hack/nvke-token.sh` wraps steps 1–3: it reads the audience off the running
deployment and mints for `nke-clients/nke-client`, so you don't have to keep the
audience in your head.

```bash
TOKEN="$(hack/nvke-token.sh)"
```

## What actually happened

Decode the token you just minted and you'll see three claims that matter:

```json
{
  "iss": "https://kubernetes.default.svc.cluster.local",
  "aud": ["https://nke.tilt.local"],
  "sub": "system:serviceaccount:nke-clients:nke-client"
}
```

NVKE checks each of them, in this order:

1. **`iss`**: is this an issuer I trust? It's compared against the configured
   issuer string exactly. Here it's your cluster's API server, which advertises
   itself under that name.
2. **Signature**: NVKE fetched the API server's public keys from
   `https://kubernetes.default.svc/openid/v1/jwks` at startup, and checks the
   token against them. The `kid` in your token's header names which key.
3. **`aud`**: does it name me? This is step 1's audience.
4. **`exp`**: is it still valid?
5. **`sub`**: does it match an admission rule? This is step 2's allow-list.

Steps 1–4 are **authentication**: is this token real. Step 5 is **admission**:
is this caller allowed in. They fail differently, which is why the status codes
differ. See the table below.

NVKE does no authorization beyond that. Every admitted caller can do everything.

## When it doesn't work

| What you see | What went wrong | Fix |
| --- | --- | --- |
| `401` `"no bearer token was presented"` | The header didn't arrive. | Check for a typo in `Authorization`, or an empty `$TOKEN`: `kubectl create token` failing prints to stderr and leaves you with an empty variable. |
| `401` and your token looks fine | Almost always the audience. | Re-read it with `hack/nvke-token.sh --audience` and mint again. A wrong audience and no token at all are the same 401. |
| `401` after it worked a minute ago | The token expired. | Mint another. They're short-lived by design. |
| `403` | The token is valid; the subject isn't admitted. | You're minting for the wrong service account, or the `admit` rules don't list yours. A different token won't help. |
| `200` for everything, even garbage tokens | Authentication is off, or NVKE is permissive. | Check `AUTH_CONFIG_PATH` on the deployment and `permissiveMode` in its `nke-controller-config`. |
| `error: failed to create token` | Your kubeconfig identity can't mint for that service account. | You need `create` on `serviceaccounts/token` for it. `deploy/k8s/nvke-auth-dev.yaml` grants this locally; apply it, or run `tilt up`. |

NVKE logs its reasoning for every rejection. This is usually faster than guessing:

```bash
kubectl -n nvke-controller logs deploy/nvke-controller | grep '"logger":"auth"'
```

```json
{"msg":"rejected request","reason":"audience_mismatch","issuer":"local-kubernetes","status":401,
 "error":"audience_mismatch: token has invalid claims: token has invalid audience"}
{"msg":"rejected request","reason":"not_admitted","issuer":"local-kubernetes","status":403,
 "error":"not_admitted: subject \"system:serviceaccount:nke-outsiders:nke-outsider\" from issuer \"local-kubernetes\" matches no admission rule"}
```

A successful request logs nothing, so don't read silence as failure.

### Reading your own token

Nothing about a JWT is secret except the signature, so you can just look:

```bash
TOKEN="$(hack/nvke-token.sh)"
echo "${TOKEN}" | cut -d. -f2 | python3 -c \
  'import sys,base64,json; s=sys.stdin.read().strip()
print(json.dumps(json.loads(base64.urlsafe_b64decode(s + "=" * (-len(s) % 4))), indent=2))'
```

(JWTs are base64url and unpadded, so piping straight to `base64 -d` works on
macOS and fails on GNU coreutils. The re-padding above works on both.)

Check `aud` against the audience NVKE expects and `exp` against the clock. Those
two account for most 401s.

### Proving a rejection

If you want to see the failure paths deliberately:

```bash
# 403: authenticates, then fails admission.
OUT="$(kubectl create token nke-outsider -n nke-outsiders \
  --audience=https://nke.tilt.local --duration=10m)"
curl -s -o /dev/null -w '%{http_code}\n' \
  -H "Authorization: Bearer ${OUT}" http://localhost:8080/api/clusters

# 401: never authenticates.
BAD="$(kubectl create token nke-client -n nke-clients \
  --audience=https://nke.wrong.invalid --duration=10m)"
curl -s -o /dev/null -w '%{http_code}\n' \
  -H "Authorization: Bearer ${BAD}" http://localhost:8080/api/clusters
```

Or run all four cases at once:

```bash
hack/nvke-auth-demo.sh
```

```
CASE                                 EXPECTED     ACTUAL  RESULT
---------------------------------- ---------- ----------  ------
1 no token                                401        401  PASS
2 nke-clients/nke-client                  200        200  PASS
3 wrong audience                          401        401  PASS
4 nke-outsiders/nke-outsider              403        403  PASS

all 4 checks passed
```

## From nvkectl

`nvkectl` is a caller of this API. After `nvkectl config set-site --kubeconfig`,
every controller API command mints the same identity this page uses (`nke-clients/nke-client`)
with the audience from ConfigMap `nvke-auth-config`, read from namespace
`nvke-controller`, then from `nke-controller` for a site installed before the
`nvke` rename, then from `cloud-workflow-engine`. There
is no `NKECTL_DEV_TOKEN` and no `hack/nvke-token.sh` step.

Those names are fixed in the CLI. A cluster-admin kubeconfig (Tilt's kind
cluster) can mint without extra RBAC. On a real site, grant the kubeconfig
identity the roles in
[Configuring NVKE API Authentication](configuring-api-authentication.md).

If the ConfigMap is missing, `nvkectl` sends no bearer token, the same as
authentication being off.

## Endpoints that need no token

`/healthz`, `/readyz`, `/api/version`, `/openapi.yaml`, `/openapi-public.yaml`, `/docs`, and the Swagger
UI at `/api/docs/`. Everything else needs one, including paths that don't route
anywhere, so you can't probe for endpoints without authenticating first.

This is why `curl localhost:8080/api/version` succeeding tells you nothing about
whether authentication is on.

## In scripts

`hack/nvke-token.sh` prints nothing when authentication is off and exits 0, so one
form works in both worlds:

```bash
TOKEN="$(hack/nvke-token.sh)"
curl ${TOKEN:+-H "Authorization: Bearer ${TOKEN}"} "${API_URL}/clusters"
```

The `${TOKEN:+...}` expansion drops the header entirely when `TOKEN` is empty,
rather than sending `Authorization: Bearer `. The bootstrap scripts under `hack/`
already do this, which is why they keep working with authentication either way.

Useful overrides: `NKE_AUTH_TOKEN` (use this token, skip discovery),
`NKE_AUDIENCE`, `NKE_TOKEN_SERVICE_ACCOUNT` / `NKE_TOKEN_NAMESPACE`,
`NKE_TOKEN_TTL`.

## Seeing who would be rejected, without rejecting them

If you're turning authentication on somewhere that already has callers, deploy
permissive first:

```bash
NKE_PERMISSIVE_MODE=true tilt up
```

Every request is evaluated, logged and counted, then served anyway. Watch:

```
nvke_auth_requests_total{outcome="would_reject"}
```

The `reason` and `issuer` labels tell you what was wrong and whose token it was.
When that counter stops moving, you can enforce. Permissive is a migration step,
not a setting to leave on: it accepts everything.

## See also

- [API Authentication](../README.md#api-authentication): configuration schema,
  RBAC prerequisite, the full rejection-reason table.
- [nvkectl](../../cli/README.md#authenticating-to-cwe): how the CLI mints.
- `deploy/k8s/nvke-auth-dev.yaml`: the local identities and auth config.
- `auth/authn/`: the implementation.
