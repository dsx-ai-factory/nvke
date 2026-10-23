# Configuring NVKE API Authentication

How to make NVKE require a token on its API, using an identity provider you
already run.

This is the operator's side. For getting a token and making a call once it is
on, see [Call the API](../../docs/operate/api-authentication.md#call-the-api) in Secure the NVKE API.

## What you are setting up

NVKE can require a signed JWT on every `/api/*` request, in one of these states:

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
last one gives a 403: the token was real, the caller just isn't on the list.

NVKE does no authorization. Every admitted caller can call every endpoint.

`issuers` is a list. Add an entry for each identity provider. `name` and `issuer`
are each unique in the file. An issuer's `audiences` replaces the top-level
list for that issuer. An issuer carries one of `serviceAccounts`, `spiffe`, or
`groups`.

## Before you start

You need permission to set Helm values on the NVKE release, and a cluster running
a chart version with an `apiAuth` section:

```bash
CHART=...   # your NVKE chart reference
helm show values "$CHART" | grep apiAuth
```

On an existing site, the controller runs under the name it was installed with:
`nke-controller` for a site installed before the `nvke` rename,
`cloud-workflow-engine` for anything older. Replace `nvke-controller` with that
name in every command on this page.

The commands below use `$NS` for the namespace NVKE runs in:

```bash
NS=nvke-controller   # your release namespace
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

Do not take `jwks_uri`. It is built from the API server's advertise address,
so it is a node IP or a load-balancer address that can change. Use the
in-cluster name instead, which is stable and serves the same thing:

```
https://kubernetes.default.svc/openid/v1/jwks
```

### 2. Create the identity your callers will use

NVKE admits callers by name, so the account has to exist before you name it in
the configuration. The Helm chart does not create these objects.

```bash
kubectl create namespace nke-clients
kubectl -n nke-clients create serviceaccount nke-client
```

`nvkectl` mints this pair and no other. The names `nke-clients` /
`nke-client` are fixed in the CLI, and it looks for the authentication ConfigMap
under two fixed names: `nvke-auth-config`, then `nke-auth-config` for a site
that keeps the older name by pinning `apiAuth.configMap.name`. It searches both
in namespace `nvke-controller`, then both in `nke-controller` for a site
installed before the `nvke` rename, then both in `cloud-workflow-engine`, and
uses the first it can read.

A ConfigMap it is not permitted to read does not stop the command: the search
moves to the next candidate, so a kubeconfig scoped to the pinned name alone
still works. The denial is reported only if no candidate turns out to be
readable. Any other error reading a ConfigMap stops the command. In any other
namespace, `nvkectl` finds no ConfigMap and sends no token.
Other names work for `curl` and `hack/nvke-token.sh` if you override
them; they will not work for `nvkectl`. One account per caller is still easier
to revoke when those callers are not the CLI.

Minting a token for that account needs `create` on `serviceaccounts/token` in
its namespace. A cluster administrator already has it. To let something else
mint it, such as `nvkectl` under a least-privilege kubeconfig, a CI job, or an
application that renews its own token, grant it:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: nke-client-token-minter
  namespace: nke-clients
rules:
  - apiGroups: [""]
    resources: ["serviceaccounts/token"]
    resourceNames: ["nke-client"]
    verbs: ["create"]
```

`resourceNames` holds that grant to the one account, so it does not become a way
to mint tokens for anything else in the namespace. Bind it to whoever is doing
the minting: the user or group in the kubeconfig `nvkectl` uses.

### 3. Write the configuration

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
to this deployment; its external URL is a good choice. Do not reuse your issuer
or a `kubernetes.default.svc` address: every pod in the cluster already carries
those, so the check would admit everything.

**`admit`** is the allow-list, and it is what makes this safe. Anyone who can
create a pod can have Kubernetes mint a token for their own service account
naming any audience they like. Verifying the signature is not enough on its own;
the allow-list is what stops every service account in the cluster from being an
NVKE administrator. Omitting `name` admits every service account in that
namespace.

The two CA and token paths are files Kubernetes puts in every pod, so they work
as written.

### 4. Set the chart values

```yaml
apiAuth:
  enabled: true
  permissive: true
  configMap:
    create: true
    name: nvke-auth-config
  config:
    # the YAML from step 3 goes here
    audiences: ["https://nke.example.com"]
    issuers:
      - name: local-kubernetes
        # ...
```

Start permissive. The next section covers going enforcing.

If you would rather manage the config yourself, with a sealed secret or an
existing GitOps pipeline, leave `create` at its default of `false` and put the document
in a ConfigMap named by `configMap.name`, under the key `auth.yaml`:

```bash
kubectl -n "$NS" create configmap nvke-auth-config \
  --from-file=auth.yaml=auth.yaml \
  --dry-run=client -o yaml | kubectl apply -f -
```

Set one or the other. Setting `config` without `create`, or `create` without
`config`, fails the install rather than silently ignoring one of them.

### 5. Check it came up

```bash
kubectl -n "$NS" logs deploy/nvke-controller | grep 'API authentication'
```

```json
{"msg":"fetched issuer signing keys","issuer":"local-kubernetes","keys":1}
{"msg":"API authentication is enabled","mode":"permissive","issuers":["local-kubernetes"]}
```

You want one `fetched issuer signing keys` line per issuer. If one is missing,
fix it now: while permissive, a failed fetch is only a warning; enforcing, it
stops NVKE from starting at all.

### 6. Tell callers what to ask for

`nvkectl` mints on its own once `config set-site --kubeconfig` points at this
cluster. The kubeconfig identity needs the Role above, the service
account must exist, and the `admit` rule must list `nke-clients/nke-client`.

For `curl` and scripts:

```bash
kubectl create token nke-client -n nke-clients \
  --audience=https://nke.example.com --duration=30m
```

This has to be `kubectl create token`. The older approach, reading a
ServiceAccount `Secret`, produces a token with no audience, which NVKE refuses.

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

If the only usable name is an IP address, use it, but know you're pinning
something that may move.

### 2. Get the CA certificate into NVKE

SPIRE's endpoint uses its own certificate, not one from a public CA, so NVKE
needs the CA to trust it:

```bash
kubectl -n spire-server get secret oidc-provider-upstream-ca \
  -o jsonpath='{.data.tls\.crt}' | base64 -d > spire-ca.crt
```

NVKE reads it from a file inside its container. It goes beside the configuration,
under `extraFiles`: the whole ConfigMap is mounted as a directory, so anything
listed here is a file next to `auth.yaml`:

```yaml
apiAuth:
  configMap:
    create: true
    extraFiles:
      spire-ca.crt: |
        -----BEGIN CERTIFICATE-----
        ...
        -----END CERTIFICATE-----
```

It is then at `/etc/nke/auth/spire-ca.crt`, which is what `caBundlePath` points
at below. A CA bundle is a public certificate, not a secret.

If you manage the ConfigMap yourself instead, put the file in it directly:

```bash
kubectl -n "$NS" create configmap nvke-auth-config \
  --from-file=auth.yaml=auth.yaml \
  --from-file=spire-ca.crt=spire-ca.crt \
  --dry-run=client -o yaml | kubectl apply -f -
```

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

An issuer uses one of `serviceAccounts`, `spiffe`, or `groups`.

### 4. Tell callers what to ask for

```bash
spire-server jwt mint \
  -spiffeID spiffe://example.org/nke-service/my-service \
  -audience https://nke.example.com \
  -ttl 10m
```

`-ttl` takes a duration like `10m`, not a number of seconds.

## Recipe: an external identity provider

Use this when callers get tokens from an identity provider you already run.
NVKE reads group membership only from the token's `groups` claim.

### 1. Write the issuer

Add this issuer to a configuration that already contains at least one top-level `audiences` value. The block below is not a complete configuration. NVKE refuses to start when `audiences` is missing.

```yaml
issuers:
  - name: example-idp
    issuer: "https://idp.example.com/realms/example"
    jwks:
      uri: "https://idp.example.com/keys"
    algorithms: [RS256]
    admit:
      groups:
        - nke-operators
```

A token is admitted when its `groups` claim contains any listed group. The
group names are matched exactly.

When the identity provider's certificate is signed by a private CA, put the
bundle in `extraFiles` and set `jwks.caBundlePath`, as in the SPIRE recipe.
Omit `caBundlePath` when a public CA signs the certificate.

## Going enforcing without breaking callers

Do not skip permissive. It is the only way to find callers you did not know
about.

Watch the counter, not the log. Successful reads are logged at debug level,
so the log will undercount them:

```bash
kubectl -n "$NS" exec deploy/nvke-controller -- \
  wget -qO- http://127.0.0.1:9090/metrics | grep '^nvke_auth_requests_total'
```

```
nvke_auth_requests_total{issuer="local-kubernetes",outcome="authenticated"} 42
nvke_auth_requests_total{issuer="unknown",outcome="would_reject",reason="no_token"} 7
```

Every `would_reject` series is a caller that enforcing would break. The `reason`
label says what each one needs: usually a token, or a subject added to your
allow-list. Rejections are also logged in full:

```bash
kubectl -n "$NS" logs deploy/nvke-controller | grep '"logger":"auth"'
```

When `would_reject` stops climbing, set `apiAuth.permissive: false`.

Add new issuers while still permissive. Enforcing NVKE will not start if it
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
| `issuers[].jwks.caBundlePath` | no | CA file inside the container, usually one you listed under `apiAuth.configMap.extraFiles`. Omit for a publicly trusted certificate. |
| `issuers[].jwks.bearerTokenPath` | no | Token file, if the keys endpoint needs one. Re-read on every fetch. |
| `issuers[].algorithms` | yes | `RS256` and `ES256` cover most issuers. Asymmetric only. |
| `issuers[].admit` | yes | One of `serviceAccounts`, `spiffe`, or `groups`. |

The file is read once at startup, so restart NVKE after you change it:

```bash
kubectl -n "$NS" rollout restart deploy/nvke-controller
```

Values are matched exactly: leading or trailing whitespace is an error, not
something NVKE trims.

### Chart values

| Value | Default | Notes |
|---|---|---|
| `apiAuth.enabled` | `false` | |
| `apiAuth.permissive` | `false` | |
| `apiAuth.configMap.name` | `nvke-auth-config` | Mounted either way. |
| `apiAuth.configMap.create` | `false` | Build the ConfigMap from `apiAuth.config`. |
| `apiAuth.configMap.extraFiles` | empty | Files to place beside `auth.yaml`, keyed by filename. A CA bundle goes here. |
| `apiAuth.config` | empty | The document, `audiences` and `issuers` only. |
| `apiAuth.clusterIssuerDiscovery` | `true` | Lets NVKE read the cluster's own keys. Already granted on a stock cluster; leave it on unless you bind that permission yourself. |

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
| `jwks_stale` | NVKE could not refresh the issuer's keys. Your problem, not the caller's. |
| `not_admitted` | Token is valid; the subject is not on the allow-list. |

Responses name the reason and nothing else. Issuer names and configuration stay
in the log.

### Endpoints that need no token

`/healthz`, `/readyz`, `/api/version`, `/openapi.yaml`, `/openapi-public.yaml`, `/docs`, and the Swagger
UI under `/api/docs/`.

Everything else needs one, including paths that route nowhere, so a caller
cannot probe for endpoints without authenticating. This is also why a successful
call to `/api/version` tells you nothing about whether authentication is on.

## Troubleshooting

**NVKE will not start, and the log says it could not set up authentication.**
When enforcing, every issuer's keys must be reachable at startup. The message
names the issuer and the error. Check the keys URL is reachable from inside the
pod, and that any `caBundlePath` points at a file that is actually mounted. If
it sits under `/etc/nke/auth`, it has to be listed in `extraFiles`.

**Everything returns 401, including tokens that look right.**
Check the audience first: it's the usual cause. A token minted for the wrong
audience is rejected identically to no token at all. Compare its `aud` claim
against the audiences NVKE logs at startup.

**A caller gets 403.**
Its token is fine and its subject is not on the allow-list. The log names the
subject; compare it to your `admit` rules. Re-minting the token will not help.

**NVKE rejected the configuration at startup.**
The error names the offending field. Usual causes: an audience that matches one
of your issuers, a value with stray whitespace, `serviceAccounts` and `spiffe`
on the same issuer, or a keys URL that is not `https`.

**Config changes have no effect.**
It is read once at startup. Restart the deployment.

**You cannot tell whether authentication is on.**
Read it off the deployment rather than probing the API:

```bash
kubectl -n "$NS" get deploy nvke-controller \
  -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="AUTH_CONFIG_PATH")].value}{"\n"}'
```

## See also

- [Secure the NVKE API](../../docs/operate/api-authentication.md): the operator path, and the caller's side including `nvkectl`.
