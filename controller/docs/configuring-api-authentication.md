# Configuring NKE API Authentication

How to make NKE require a token on its API, using an identity provider you
already run.

This is the operator's side. For getting a token and making a call once it is
on, see [Authenticating to the NKE API](authenticating-to-the-api.md).

## What you are setting up

NKE can require a signed JWT on every `/api/*` request. There are three states:

| State | Behaviour |
|---|---|
| **Off** | Every request is served. This is the default. |
| **Permissive** | Every request is checked and counted, then served anyway. Nothing is rejected. |
| **Enforcing** | Failed checks are rejected. |

Permissive exists so you can turn this on somewhere that already has callers and
find out who would break before they do. Use it.

A token is accepted when its issuer is one you configured, its signature
verifies, it names your deployment in its `aud` claim, it has not expired, and
its subject matches a rule you wrote. The first four failing gives a 401. The
last one gives a 403 — the token was real, the caller just is not on the list.

NKE does no authorization. Every admitted caller can call every endpoint.

## Before you start

You need permission to set Helm values on the NKE release, and a cluster running
a chart version with an `apiAuth` section:

```bash
CHART=...   # your NKE chart reference
helm show values "$CHART" | grep apiAuth
```

The commands below use `$NS` for the namespace NKE runs in:

```bash
NS=cloud-workflow-engine   # your release namespace
```

## Recipe: your cluster's own API server

This is the common case, and it needs no extra infrastructure. Kubernetes
already signs tokens for service accounts and publishes the keys to verify them.

### 1. Read your cluster's issuer

Read it rather than assuming it:

```bash
kubectl get --raw /.well-known/openid-configuration
```

```json
{
  "issuer": "https://kubernetes.default.svc.cluster.local",
  "jwks_uri": "https://10.0.0.1:6443/openid/v1/jwks"
}
```

Take the `issuer` value.

**Do not take `jwks_uri`.** It is built from the API server's advertise address,
so it is a node IP or a load-balancer address that can change. Use the
in-cluster name instead, which is stable and serves the same thing:

```
https://kubernetes.default.svc/openid/v1/jwks
```

### 2. Write the configuration

```yaml
audiences: ["https://nke.example.com"]
issuers:
  - name: local-kubernetes
    issuer: "https://kubernetes.default.svc.cluster.local"
    jwks:
      uri: "https://kubernetes.default.svc/openid/v1/jwks"
      caBundlePath: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt
      bearerTokenPath: /var/run/secrets/kubernetes.io/serviceaccount/token
    algorithms: [RS256, ES256]
    admit:
      serviceAccounts:
        - namespace: nke-clients
          name: nke-client
```

**`audiences`** is what callers must name in their token. Pick something unique
to this deployment — its external URL is a good choice. Do not reuse your issuer
or a `kubernetes.default.svc` address: every pod in the cluster already carries
those, so the check would admit everything.

**`admit`** is the allow-list, and it is what makes this safe. Anyone who can
create a pod can have Kubernetes mint a token for their own service account
naming any audience they like. Verifying the signature is not enough on its own;
the allow-list is what stops every service account in the cluster from being an
NKE administrator. Omitting `name` admits every service account in that
namespace.

The two CA and token paths are files Kubernetes puts in every pod, so they work
as written.

### 3. Set the chart values

```yaml
apiAuth:
  enabled: true
  permissive: true
  configMap:
    create: true
    name: nke-auth-config
  config:
    # the YAML from step 2 goes here
    audiences: ["https://nke.example.com"]
    issuers:
      - name: local-kubernetes
        # ...
```

Start permissive. The next section covers going enforcing.

If you would rather manage the config yourself — a sealed secret, an existing
GitOps pipeline — leave `create` at its default of `false` and put the document
in a ConfigMap named by `configMap.name`, under the key `auth.yaml`:

```bash
kubectl -n "$NS" create configmap nke-auth-config \
  --from-file=auth.yaml=auth.yaml \
  --dry-run=client -o yaml | kubectl apply -f -
```

Set one or the other. Setting `config` without `create`, or `create` without
`config`, fails the install rather than silently ignoring one of them.

### 4. Check it came up

```bash
kubectl -n "$NS" logs deploy/cloud-workflow-engine | grep 'API authentication'
```

```json
{"msg":"fetched issuer signing keys","issuer":"local-kubernetes","keys":1}
{"msg":"API authentication is enabled","mode":"permissive","issuers":["local-kubernetes"]}
```

You want one `fetched issuer signing keys` line per issuer. If one is missing,
fix it now — while permissive, a failed fetch is only a warning, but when
enforcing it stops NKE from starting at all.

### 5. Tell callers what to ask for

```bash
kubectl create token nke-client -n nke-clients \
  --audience=https://nke.example.com --duration=30m
```

This has to be `kubectl create token`. The older approach — reading a
ServiceAccount `Secret` — produces a token with no audience, which NKE refuses.

## Recipe: SPIRE

Use this if you already run SPIRE. The shape is the same; the differences are
that subjects are SPIFFE IDs rather than service accounts, and you have to
supply a CA certificate yourself.

### 1. Read the issuer, and check the certificate

```bash
kubectl -n spire-server exec deploy/spire-spiffe-oidc-discovery-provider -- \
  curl -sk https://127.0.0.1/.well-known/openid-configuration
```

As above, take `issuer` and be careful about the keys URL. Whatever address you
choose has to appear in the certificate SPIRE serves, or the connection fails:

```bash
kubectl -n spire-server get secret spire-spiffe-oidc-discovery-provider-cert \
  -o jsonpath='{.data.tls\.crt}' | base64 -d | \
  openssl x509 -noout -ext subjectAltName
```

If the only usable name is an IP address, use it — but know that you are pinning
something that may move.

### 2. Get the CA certificate into NKE

SPIRE's endpoint uses its own certificate, not one from a public CA, so NKE
needs the CA to trust it:

```bash
kubectl -n spire-server get secret oidc-provider-upstream-ca \
  -o jsonpath='{.data.tls\.crt}' | base64 -d > spire-ca.crt
```

NKE reads it from a file inside its container, so it has to be mounted there.
The simplest way is to put it in the same ConfigMap as the config — NKE mounts
that whole ConfigMap as a directory, so the certificate appears alongside:

```bash
kubectl -n "$NS" create configmap nke-auth-config \
  --from-file=auth.yaml=auth.yaml \
  --from-file=spire-ca.crt=spire-ca.crt \
  --dry-run=client -o yaml | kubectl apply -f -
```

It is then at `/etc/nke/auth/spire-ca.crt`.

> **This means you cannot use `configMap.create: true` with SPIRE.** A ConfigMap
> the chart builds holds the config file and nothing else, so there is nowhere
> to put the certificate. Either create the ConfigMap yourself as above, or put
> the certificate in its own ConfigMap and mount it with the chart's `volumes`
> and `volumeMounts` values. The same applies to any issuer whose keys endpoint
> uses a private CA.

### 3. Write the issuer

```yaml
issuers:
  - name: spire
    issuer: "https://spire.example.com"
    jwks:
      uri: "https://spire.example.com:30443/keys"
      caBundlePath: /etc/nke/auth/spire-ca.crt
    algorithms: [RS256, ES256, ES384]
    admit:
      spiffe:
        trustDomain: example.org
        pathPrefixes: ["/nke-service/"]
```

`pathPrefixes` is required and cannot be `/`. A trust domain on its own would
admit every workload in it, which is rarely what you want.

An issuer uses either `serviceAccounts` or `spiffe`, never both.

### 4. Tell callers what to ask for

```bash
spire-server jwt mint \
  -spiffeID spiffe://example.org/nke-service/my-service \
  -audience https://nke.example.com \
  -ttl 10m
```

`-ttl` takes a duration like `10m`, not a number of seconds.

## Going enforcing without breaking callers

Do not skip permissive. It is the only way to find callers you did not know
about.

**Watch the counter, not the log.** Successful reads are logged at debug level,
so the log will undercount them:

```bash
kubectl -n "$NS" exec deploy/cloud-workflow-engine -- \
  curl -s http://127.0.0.1:9090/metrics | grep '^nke_auth_requests_total'
```

```
nke_auth_requests_total{issuer="local-kubernetes",outcome="authenticated"} 42
nke_auth_requests_total{issuer="unknown",outcome="would_reject",reason="no_token"} 7
```

Every `would_reject` series is a caller that enforcing would break. The `reason`
label says what each one needs — usually a token, or a subject added to your
allow-list. Rejections are also logged in full:

```bash
kubectl -n "$NS" logs deploy/cloud-workflow-engine | grep '"logger":"auth"'
```

When `would_reject` stops climbing, set `apiAuth.permissive: false`.

Add new issuers while still permissive. Enforcing NKE will not start if it
cannot reach every issuer's keys, so a bad issuer becomes an outage rather than
a warning.

## Reference

### Configuration fields

| Field | Required | Notes |
|---|---|---|
| `audiences` | yes | What callers must put in `aud`. Any-of. List two to rotate without downtime. |
| `issuers[].name` | yes | Appears in logs and metrics. |
| `issuers[].issuer` | yes | Must match the token's `iss` exactly. |
| `issuers[].audiences` | no | Overrides the top-level list for this issuer. |
| `issuers[].jwks.uri` | yes | Must be `https`. |
| `issuers[].jwks.caBundlePath` | no | CA file inside the container. Omit for a publicly trusted certificate. |
| `issuers[].jwks.bearerTokenPath` | no | Token file, if the keys endpoint needs one. Re-read on every fetch. |
| `issuers[].algorithms` | yes | `RS256` and `ES256` cover most issuers. Asymmetric only. |
| `issuers[].admit` | yes | `serviceAccounts` or `spiffe`, not both. |

The file is read once at startup. The chart restarts NKE when the config
changes; if yours does not, restart it yourself:

```bash
kubectl -n "$NS" rollout restart deploy/cloud-workflow-engine
```

Values are matched exactly — leading or trailing whitespace is an error, not
something NKE trims.

### Chart values

| Value | Default | Notes |
|---|---|---|
| `apiAuth.enabled` | `false` | |
| `apiAuth.permissive` | `false` | |
| `apiAuth.configMap.name` | `nke-auth-config` | Mounted either way. |
| `apiAuth.configMap.create` | `false` | Build the ConfigMap from `apiAuth.config`. |
| `apiAuth.config` | empty | The document, `audiences` and `issuers` only. |
| `apiAuth.clusterIssuerDiscovery` | `true` | Lets NKE read the cluster's own keys. Already granted on a stock cluster; leave it on unless you bind that permission yourself. |

### Why a request was rejected

Everything is a 401 except `not_admitted`, which is a 403.

| `reason` | Meaning |
|---|---|
| `no_token` | No `Authorization` header. |
| `unknown_issuer` | `iss` matches no configured issuer. |
| `bad_signature` | Did not verify, or is not a well-formed JWT. |
| `expired` | Past its `exp`. |
| `not_yet_valid` | Its `nbf` is in the future. |
| `audience_mismatch` | `aud` does not name this deployment. |
| `algorithm_not_allowed` | Signed with an algorithm you did not list. |
| `jwks_stale` | NKE could not refresh the issuer's keys. Your problem, not the caller's. |
| `not_admitted` | Token is valid; the subject is not on the allow-list. |

Responses name the reason and nothing else. Issuer names and configuration stay
in the log.

### Endpoints that need no token

`/healthz`, `/readyz`, `/api/version`, `/openapi.yaml`, `/docs`, and the Swagger
UI under `/api/docs/`.

Everything else needs one, including paths that route nowhere, so a caller
cannot probe for endpoints without authenticating. This is also why a successful
call to `/api/version` tells you nothing about whether authentication is on.

## Troubleshooting

**NKE will not start, and the log says it could not set up authentication.**
When enforcing, every issuer's keys must be reachable at startup. The message
names the issuer and the error. Check the keys URL is reachable from inside the
pod, and that any `caBundlePath` points at a file that is actually mounted.

**Everything returns 401, including tokens that look right.**
Check the audience first — it is the usual cause. A token minted for the wrong
audience is rejected identically to no token at all. Compare its `aud` claim
against the audiences NKE logs at startup.

**A caller gets 403.**
Its token is fine and its subject is not on the allow-list. The log names the
subject; compare it to your `admit` rules. Re-minting the token will not help.

**NKE rejected the configuration at startup.**
The error names the offending field. Usual causes: an audience that matches one
of your issuers, a value with stray whitespace, `serviceAccounts` and `spiffe`
on the same issuer, or a keys URL that is not `https`.

**Config changes have no effect.**
It is read once at startup. Restart the deployment.

**You cannot tell whether authentication is on.**
Read it off the deployment rather than probing the API:

```bash
kubectl -n "$NS" get deploy cloud-workflow-engine \
  -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="AUTH_CONFIG_PATH")].value}{"\n"}'
```

## See also

- [Authenticating to the NKE API](authenticating-to-the-api.md) — the caller's side.
