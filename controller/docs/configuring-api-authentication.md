# Configuring NKE API Authentication

How to turn on API authentication for a deployment you operate.

This guide covers writing the configuration, deploying it, and moving a running
site from permissive to enforcing. For getting a token and making a call, see
[Authenticating to the NKE API](authenticating-to-the-api.md).

## 1. Overview

NKE can require a signed JWT on every `/api/*` request. There are three states:

| State | `AUTH_CONFIG_PATH` | `PERMISSIVE_MODE` | Behaviour |
|---|---|---|---|
| Disabled | unset | ignored | Every request is served. This is the default. |
| Permissive | set | `true` | Every request is checked, logged and counted, then served anyway. Nothing is rejected. |
| Enforcing | set | `false` or unset | Failed checks are rejected with 401 or 403. |

A token is accepted when all of the following are true:

1. Its `iss` claim matches a configured issuer.
2. Its signature verifies against that issuer's public keys.
3. Its `aud` claim contains one of the configured audiences.
4. It has not expired.
5. Its `sub` claim matches an admission rule for that issuer.

Steps 1 to 4 are authentication. Step 5 is admission. They fail differently:
a failure in 1 to 4 returns 401, and a failure in 5 returns 403.

NKE performs no authorization. Every admitted caller can call every endpoint.

## 2. Prerequisites

- A cluster running NKE from a chart version that has the `apiAuth` values.
  Check with `helm show values <chart> | grep apiAuth`.
- Permission to create a ConfigMap in NKE's namespace.
- For the SPIRE recipe, access to the SPIRE server and its CA.

## 3. The configuration file

One YAML file. The chart mounts it at `/etc/nke/auth/auth.yaml` and sets
`AUTH_CONFIG_PATH` to that path.

```yaml
apiVersion: auth.nke.nvidia.com/v1alpha1
kind: AuthenticationConfiguration
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

`apiVersion` and `kind` are required and must be exactly the values above.
The Kubernetes API server has its own `AuthenticationConfiguration` kind in a
different group. This is not that.

The file is read once, at startup. Changing it requires a pod restart.

No field may have leading or trailing whitespace. NKE rejects padded values
rather than trimming them.

## 4. Audiences

`audiences` is the list of values NKE accepts in a token's `aud` claim. At least
one is required. Matching is any-of.

Pick a value specific to this deployment. The Teleport application address or
external URL is a good choice.

```yaml
audiences: ["https://nke.example.com"]
```

Three values are rejected at startup:

| Rejected | Reason |
|---|---|
| Anything containing `*` | Audiences are compared literally. A wildcard would match no token at all. |
| A value equal to a configured issuer | On a kubeadm cluster `--api-audiences` defaults to the service account issuer, so every pod token already carries it. |
| A `kubernetes.default.svc` form | Same reason. Every automounted pod token carries it, so the check would admit the whole cluster. |

List two audiences to rotate without downtime. Add the new one, move clients,
then remove the old one.

## 5. Issuers

At least one issuer is required. Each needs a unique `name` and a unique
`issuer`.

| Field | Required | Description |
|---|---|---|
| `name` | yes | Label used in logs and metrics. Unique within the file. |
| `issuer` | yes | Must equal the token's `iss` claim exactly. Unique within the file. |
| `audiences` | no | Overrides the deployment-wide list for this issuer. Absent means inherit. An empty list is an error. |
| `jwks` | yes | Where to fetch this issuer's public keys. |
| `algorithms` | yes | Signing algorithms accepted from this issuer. |
| `admit` | yes | Which subjects this issuer may authenticate. |

### 5.1 jwks

```yaml
jwks:
  uri: "https://kubernetes.default.svc/openid/v1/jwks"
  caBundlePath: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt
  bearerTokenPath: /var/run/secrets/kubernetes.io/serviceaccount/token
```

| Field | Required | Description |
|---|---|---|
| `uri` | yes | JWKS endpoint. Must be absolute and use `https`. |
| `caBundlePath` | no | PEM file for the endpoint's certificate chain. Absent means system roots. |
| `bearerTokenPath` | no | File containing a bearer token for the fetch. Re-read on every fetch, so a rotated token keeps working. |

Both paths are paths inside the NKE container.

Do not copy `jwks_uri` from an OIDC discovery document. That value is often a
node IP or a VIP. Use a stable address instead. See section 7.

### 5.2 algorithms

Required. Asymmetric algorithms only:

```
RS256  RS384  RS512
PS256  PS384  PS512
ES256  ES384  ES512
EdDSA
```

List only what the issuer actually uses.

## 6. Admission rules

`admit` decides which subjects an issuer may authenticate. At least one rule is
required.

Verifying a token is not enough on its own. Anyone who can create a pod can have
the kubelet project a token for their own service account, naming any audience
they choose. No RBAC check applies to that. Admission rules are what stop every
service account in the cluster from being an NKE administrator.

There are two rule kinds. **An issuer may use one or the other, never both.**
Either kind admits on a match, so setting both restricts nothing. NKE rejects a
configuration that sets both.

### 6.1 serviceAccounts

```yaml
admit:
  serviceAccounts:
    - namespace: nke-clients
      name: nke-client
```

| Field | Required | Description |
|---|---|---|
| `namespace` | yes | Must be a valid Kubernetes namespace name. |
| `name` | no | Must be a valid service account name. **Omitting it admits every service account in the namespace.** |

### 6.2 spiffe

```yaml
admit:
  spiffe:
    trustDomain: example.org
    pathPrefixes: ["/nke-service/"]
```

| Field | Required | Description |
|---|---|---|
| `trustDomain` | yes | A bare trust domain. No `spiffe://` scheme, no path. |
| `pathPrefixes` | yes | At least one. Each must start with `/`. |

Prefixes match segment-wise. `/nke-service/` admits
`spiffe://example.org/nke-service/api` but not
`spiffe://example.org/nke-service-other/api`. A trailing slash excludes the bare
segment itself.

`pathPrefixes` is required because a trust domain alone would admit every
workload in it. A prefix of `/` is rejected for the same reason.

## 7. Recipe: the cluster's own API server

The Kubernetes API server signs service account tokens and publishes its public
keys. No extra infrastructure is needed.

### 7.1 Read the issuer

Do not assume the value. Read it:

```bash
kubectl get --raw /.well-known/openid-configuration
```

```json
{
  "issuer": "https://kubernetes.default.svc.cluster.local",
  "jwks_uri": "https://10.0.0.1:6443/openid/v1/jwks"
}
```

Use the `issuer` value. **Do not use `jwks_uri`.** It is derived from the API
server's advertise address, so it is a node IP or a VIP. Use the in-cluster
Service name instead, which is stable and serves the same endpoint:

```
https://kubernetes.default.svc/openid/v1/jwks
```

### 7.2 Write the issuer block

```yaml
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

The endpoint requires authentication, which is what `bearerTokenPath` is for.
Kubernetes grants read access to the `system:serviceaccounts` group by default,
so a stock cluster needs no extra RBAC. If your cluster has narrowed that
default, set `apiAuth.clusterIssuerDiscovery=true` so the chart creates an
explicit binding.

### 7.3 Clients

Clients mint tokens with the TokenRequest API:

```bash
kubectl create token nke-client -n nke-clients \
  --audience=https://nke.example.com --duration=30m
```

A legacy ServiceAccount `Secret` will not work. It carries no audience.

## 8. Recipe: SPIRE

SPIRE issues JWT-SVIDs. The subject is a SPIFFE ID, so use a `spiffe` admission
rule.

### 8.1 Read the issuer and JWKS URI

```bash
kubectl -n spire-server exec deploy/spire-spiffe-oidc-discovery-provider -- \
  curl -sk https://127.0.0.1/.well-known/openid-configuration
```

Use the `issuer` value. As in section 7, do not use the advertised `jwks_uri`
unless you have confirmed the address is stable and appears in the serving
certificate's SANs.

Check the certificate before choosing a URI:

```bash
kubectl -n spire-server get secret spire-spiffe-oidc-discovery-provider-cert \
  -o jsonpath='{.data.tls\.crt}' | base64 -d | \
  openssl x509 -noout -ext subjectAltName
```

The URI you pick must appear in that output, or TLS verification fails.

### 8.2 Get the CA

The discovery provider's certificate is not signed by a public CA:

```bash
kubectl -n spire-server get secret oidc-provider-upstream-ca \
  -o jsonpath='{.data.tls\.crt}' | base64 -d > spire-ca.crt
```

Deliver it to the NKE container. The simplest way is a second key in the same
ConfigMap, because the chart mounts the whole ConfigMap at `/etc/nke/auth`:

```bash
kubectl -n cloud-workflow-engine create configmap nke-auth-config \
  --from-file=auth.yaml=auth.yaml \
  --from-file=spire-ca.crt=spire-ca.crt \
  --dry-run=client -o yaml | kubectl apply -f -
```

The file is then at `/etc/nke/auth/spire-ca.crt`.

### 8.3 Write the issuer block

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

### 8.4 Clients

```bash
spire-server jwt mint \
  -spiffeID spiffe://example.org/nke-service/my-service \
  -audience https://nke.example.com \
  -ttl 10m
```

`-ttl` takes a duration such as `10m`, not a number of seconds.

## 9. Deploying and enabling

### 9.1 Create the ConfigMap

The chart mounts this ConfigMap but does not create it. The contents are
per-deployment.

```bash
kubectl -n cloud-workflow-engine create configmap nke-auth-config \
  --from-file=auth.yaml=auth.yaml \
  --dry-run=client -o yaml | kubectl apply -f -
```

The key must be `auth.yaml`.

### 9.2 Set the chart values

```yaml
apiAuth:
  enabled: true
  permissive: true
  configMap:
    name: nke-auth-config
  clusterIssuerDiscovery: true
```

Start with `permissive: true`. Section 10 covers the move to enforcing.

### 9.3 Confirm it loaded

```bash
kubectl -n cloud-workflow-engine logs deploy/cloud-workflow-engine \
  | grep -E 'API authentication|fetched issuer signing keys'
```

```json
{"msg":"fetched issuer signing keys","issuer":"local-kubernetes","keys":1}
{"msg":"API authentication is enabled","mode":"permissive","issuers":["local-kubernetes"]}
```

Expect one `fetched issuer signing keys` line per issuer. If one is missing, fix
it now. In permissive mode a failed fetch is a warning. In enforcing mode it
prevents startup.

## 10. Migrating to enforcing

Do not skip permissive. It exists so you can find callers that would break
before they do.

### 10.1 Deploy permissive

Set `apiAuth.permissive: true`. Nothing is rejected.

### 10.2 Find the callers

Metrics are on a separate listener, `0.0.0.0:9090` by default:

```bash
POD=$(kubectl -n cloud-workflow-engine get pod \
  -l app.kubernetes.io/name=cloud-workflow-engine -o name | head -1)
kubectl -n cloud-workflow-engine exec "$POD" -c cloud-workflow-engine -- \
  curl -s http://127.0.0.1:9090/metrics | grep '^nke_auth_requests_total'
```

```
nke_auth_requests_total{issuer="local-kubernetes",outcome="authenticated",reason=""} 42
nke_auth_requests_total{issuer="unknown",outcome="would_reject",reason="no_token"} 7
```

Every `outcome="would_reject"` series is a caller that enforcing would break.
The `reason` and `issuer` labels say what each one needs.

Use the counter, not the request log. NKE logs successful GET requests at debug
level, so the log undercounts reads.

The rejection log gives you the detail:

```bash
kubectl -n cloud-workflow-engine logs deploy/cloud-workflow-engine \
  | grep '"logger":"auth"'
```

### 10.3 Fix each caller

Give each one a token, or add its subject to an admission rule. Adding a rule
requires a pod restart.

### 10.4 Enforce

When `would_reject` stops increasing, set `apiAuth.permissive: false`.

Enforcing is fail-closed at startup. If any issuer's keys cannot be fetched, NKE
exits rather than starting. Add and verify new issuers while still permissive.

## 11. Reference

### 11.1 Environment variables

| Variable | Default | Description |
|---|---|---|
| `AUTH_CONFIG_PATH` | unset | Path to the configuration file. Unset disables authentication. Set but unreadable is a startup failure, not "disabled". |
| `PERMISSIVE_MODE` | `false` | Only read when `AUTH_CONFIG_PATH` is set. |
| `METRICS_LISTEN_ADDRESS` | `0.0.0.0:9090` | Where `nke_auth_requests_total` is served. |

The chart renders the first two from the `apiAuth` values.

### 11.2 Rejection reasons

Every rejection is 401 except `not_admitted`, which is 403.

| `reason` | Status | Meaning |
|---|---|---|
| `no_token` | 401 | No bearer token in the `Authorization` header. |
| `unknown_issuer` | 401 | `iss` matches no configured issuer. |
| `bad_signature` | 401 | Did not verify against the issuer's keys, or is not a well-formed JWT. |
| `expired` | 401 | `exp` is in the past, or absent. |
| `not_yet_valid` | 401 | `nbf` is in the future beyond the clock-skew allowance. |
| `audience_mismatch` | 401 | `aud` does not contain a configured audience. |
| `algorithm_not_allowed` | 401 | `alg` is not in that issuer's list. |
| `jwks_stale` | 401 | The issuer's keys could not be refreshed. This is a server problem, not a caller problem. |
| `not_admitted` | 403 | The token verified. The subject matches no admission rule. |

401 responses carry a `WWW-Authenticate: Bearer` challenge. The response body
names only the reason. Issuer names and configuration detail stay in the log.

### 11.3 Paths that need no token

```
/healthz  /readyz  /api/version  /openapi.yaml  /docs  /api/docs/
```

Everything else requires a token, including paths that route nowhere.

Because `/api/version` is on this list, a successful call to it does not tell
you whether authentication is enabled.

## 12. Troubleshooting

**NKE will not start, `Failed to set up API authentication`.**
The message names the issuer and the error. In enforcing mode every issuer's
keys must be fetchable at startup. Check the JWKS URI is reachable from inside
the pod and that `caBundlePath` points to a file that exists in the container.

**Every request returns 401, including ones with a valid-looking token.**
Check the audience first. A token minted for the wrong audience is rejected the
same way as no token at all. Compare the `aud` claim against the `audiences` in
the startup log.

**A request returns 403.**
The token verified and the subject is not admitted. The log names the subject.
Compare it against the `admit` rules. A different token will not help.

**Configuration rejected at startup.**
The error names the field and the reason. Common causes: an audience that
matches an issuer, a value with leading or trailing whitespace, `serviceAccounts`
and `spiffe` set on the same issuer, or a `jwks.uri` that is not `https`.

**Changes to the ConfigMap have no effect.**
The file is read once at startup. Restart the deployment.

**Cannot tell whether authentication is on.**
Read the deployment, not the API:

```bash
kubectl -n cloud-workflow-engine get deploy cloud-workflow-engine \
  -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="AUTH_CONFIG_PATH")].value}{"\n"}'
```

## See also

- [Authenticating to the NKE API](authenticating-to-the-api.md) — client side.
- [API Authentication](../README.md#api-authentication) — schema reference.
- `deploy/k8s/nke-auth-dev.yaml` — a working local example. Development only.
