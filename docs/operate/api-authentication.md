# Secure the NVKE API

This page describes how to make the NVIDIA Kubernetes Engine (NVKE) API require a signed JSON Web Token (JWT) from an OpenID Connect (OIDC) identity provider (IdP) that you already run. Do this after you install the management-plane services and before you give anyone else access to the API.

Authentication has three modes. Off is the default.

| Mode | Behavior |
|---|---|
| Off | Every request is served. This is the default. |
| Permissive | Every request is checked and counted, then served. Nothing is rejected. |
| Enforcing | A request without a valid token gets `401`. A valid token that no admission rule matches gets `403`. |

[NVKE does no authorization.](../overview/preview-status.md#api-and-access) Every caller that NVKE admits can call every endpoint, including the ones that create and delete workload clusters.

> **Warning:** The NVKE API serves plain HTTP on NodePort 30080, and neither NVKE nor its chart serves it over TLS. Every bearer token a caller sends to `http://<MANAGEMENT_ADDRESS>:30080` crosses the network unencrypted, and anyone who captures it can call the API until it expires. Send tokens only to a TLS endpoint that you put in front of the API, such as a reverse proxy that terminates TLS and forwards to port 30080, or admit to the API, in the NetworkPolicy from [Run NVKE end to end on one machine](../get-started/local-site.md), only clients on a trusted, isolated network.

## Before you begin

- You have completed [Install the management plane](install-management-plane.md). You can do this before or after [Configure an infrastructure provider: NICo](provider-nico.md).
- You run the install and `kubectl` commands on the host you installed from, from the root of the same `nvke/` checkout, with the site spec, `<CLUSTER_NAME>.kubeconfig` and, if you installed from one, `release.json` in the directory above it, as in [Run NVKE end to end on one machine](../get-started/local-site.md). Set `KUBECONFIG` for `kubectl`:

  ```bash
  export KUBECONFIG="$PWD/../<CLUSTER_NAME>.kubeconfig"
  ```

  The install created the Helm release `nvke-controller` and the Deployment `nvke-controller`, both in the `nvke-controller` namespace.
- The shell you run the install from has the `NVKECTL_*` variables from [Run NVKE end to end on one machine](../get-started/local-site.md) exported, as described in [Run NVKE end to end on one machine](../get-started/local-site.md).
- Your IdP publishes an OIDC discovery document over HTTPS, and the management cluster can reach its signing-key endpoint.
- Your IdP has a group for the people and automation allowed to call the NVKE API, and puts it in a token claim named `groups`, a list of strings. NVKE reads group membership only from that claim.
- Your IdP has an OIDC client for `nvkectl` that needs no client secret.
- The `kubectl oidc-login` plugin, `jq` and `python3` are installed on the workstation you call the API from.

## Collect the identity provider values

1. Read the IdP's discovery document.

   ```bash
   curl -s <ISSUER_URL>/.well-known/openid-configuration | jq '{issuer, jwks_uri, id_token_signing_alg_values_supported}'
   ```

   `<ISSUER_URL>` is your IdP's issuer URL, such as `https://idp.example.com/realms/example`.

   Record the output:

   - `issuer`: use this exact string as `<ISSUER_URL>`. NVKE compares it with the token's `iss` claim byte for byte, including any trailing slash.
   - `jwks_uri` is `<JWKS_URI>`, where NVKE fetches the signing keys. It must use `https`.
   - `id_token_signing_alg_values_supported` lists the signing algorithms. NVKE accepts only asymmetric algorithms, such as `RS256` and `ES256`.

2. Get a token from the IdP and read its claims.

   ```bash
   TOKEN="$(kubectl oidc-login get-token --oidc-issuer-url=<ISSUER_URL> --oidc-client-id=<CLIENT_ID> | jq -r .status.token)"
   ```

   ```bash
   echo "$TOKEN" | cut -d. -f2 | python3 -c 'import sys,base64,json; s=sys.stdin.read().strip(); print(json.dumps(json.loads(base64.urlsafe_b64decode(s + "=" * (-len(s) % 4))), indent=2))'
   ```

   `<CLIENT_ID>` is the ID of the OIDC client for `nvkectl`. `kubectl oidc-login` asks you to log in to the IdP the first time.

   Record two claims:

   - `aud` is `<AUDIENCE>`, the value callers must present. For a token from `kubectl oidc-login` it is usually `<CLIENT_ID>`.
   - `groups` must contain the group you will admit, `<ADMITTED_GROUP>`. If it is missing, configure the IdP to release it. Some IdPs release it only when the client asks for a scope such as `groups`; pass that with `--oidc-extra-scope`. Use the value exactly as the claim shows it.

## Turn on authentication in permissive mode

1. Write the Helm values to a file named `auth-values.yaml`, in the directory above the `nvke/` checkout.

   ```yaml
   apiAuth:
     enabled: true
     permissive: true
     configMap:
       create: true
     config:
       audiences: ["<AUDIENCE>"]
       issuers:
         - name: example-idp
           issuer: "<ISSUER_URL>"
           jwks:
             uri: "<JWKS_URI>"
           algorithms: [RS256]
           admit:
             groups:
               - <ADMITTED_GROUP>
   ```

   - `audiences`: every `aud` value NVKE accepts. A token is accepted if it names any of them, so list a second value for callers whose tokens carry a different audience. An audience must not equal an issuer URL.
   - `name`: a label of your choice. It appears in logs and in the `issuer` label of the metrics.
   - `algorithms`: the algorithms from step 1 that you accept.
   - `admit.groups`: a token is admitted if its `groups` claim contains any listed group.

   `issuers` is a list. Add another entry for a second IdP. `name` and `issuer` are unique in the file. Set `audiences` on an issuer to replace the top-level `audiences` for that issuer.

   NVKE uses each value exactly as written. A value with leading or trailing whitespace stops NVKE from starting.

2. If the IdP's certificate is signed by a private certificate authority (CA), add the CA certificate to the same file, inline in the issuer's `jwks`. A CA certificate is public, so it needs no Secret.

   ```yaml
   apiAuth:
     config:
       issuers:
         - name: example-idp
           jwks:
             caBundle: |
               -----BEGIN CERTIFICATE-----
               ...
               -----END CERTIFICATE-----
   ```

   Merge `caBundle` into the existing issuer's `jwks` from step 1, rather than adding a second `apiAuth` block or issuer. `caBundle` must contain at least one PEM certificate. Without it, NVKE trusts the system CAs.

   To keep the certificate as a file instead, list it under `apiAuth.configMap.extraFiles`, which places it in `/etc/nke/auth/`, and point `jwks.caBundlePath` at it, for example `/etc/nke/auth/idp-ca.crt`. Set `caBundle` or `caBundlePath`, not both.

3. Run the install command from [Run NVKE end to end on one machine](../get-started/local-site.md) again, with the same flags, including any `--values-for nktp=<FILE>`, and `--values-for controller=../auth-values.yaml` added.

   ```bash
   "$NVKE_REPO"/deploy/nvke-deploy-local.sh install \
     --values-for controller=../auth-values.yaml
   ```

   The wrapper reads the site spec, the stack config and the kubeconfig from `NVKE_SITESPEC`, `NVKE_STACK_CONFIG` and `NVKE_KUBECONFIG`, which `env.sh` exports, so the controller chart and image stay the ones you installed. The install upgrades each component in place, and applies `auth-values.yaml` to the controller after the deploy path's own values, which turn authentication off. The chart refuses to render if `config` is set without `create`, if `config` carries `enabled` or `permissive`, which belong directly under `apiAuth`, or if `caBundlePath` names a file under `/etc/nke/auth` that `extraFiles` does not list.

4. Wait for the new pods and check the startup log.

   ```bash
   kubectl -n nvke-controller rollout status deploy/nvke-controller
   ```

   ```bash
   kubectl -n nvke-controller logs deploy/nvke-controller | grep -E 'fetched issuer signing keys|API authentication'
   ```

   The output contains a `fetched issuer signing keys` line for `example-idp` and an `API authentication is enabled` line with `"mode":"permissive"`.

   If the keys line is missing, look for a `JWKS fetch failed` warning and fix the URI, the CA bundle or the network path now. In permissive mode NVKE starts without the keys, counts that issuer's tokens as `jwks_stale`, and retries as they arrive. In enforcing mode it does not start.

## Call the API

Configure your callers now, while NVKE still serves every request. From the moment the install creates the authentication configuration, `nvkectl` configured with `<CLUSTER_NAME>.kubeconfig` fails before it sends a request, with an error that contains `minting token for nke-clients/nke-client`: with that kubeconfig it requests a token for a service account that the install does not create.

### With nvkectl

1. Generate a kubeconfig that gets tokens from your IdP.

   ```bash
   nvkectl generate oidc-kubeconfig --kubeconfig ../<CLUSTER_NAME>.kubeconfig \
     --oidc-issuer-url <ISSUER_URL> \
     --oidc-client-id <CLIENT_ID> \
     --output-file ../oidc.kubeconfig
   ```

   The command copies the cluster address and CA from `<CLUSTER_NAME>.kubeconfig` and replaces the user with `kubectl oidc-login`. Add `--oidc-extra-scope <SCOPE>` for each scope your IdP needs to release the `groups` claim. On a machine with no browser, add `--headless`. `kubectl oidc-login` then prints a URL to open on another machine. The IdP client must allow the device authorization grant.

2. Point `nvkectl` at the API with that kubeconfig.

   ```bash
   nvkectl config set-site --name <CLUSTER_NAME> --url <API_URL> --kubeconfig "$PWD/../oidc.kubeconfig"
   ```

   `nvkectl` stores the kubeconfig path as you give it, so give an absolute path; a relative one works only from the directory you ran `set-site` in.

   `<API_URL>` is the base address of the NVKE API without `/api`: `http://<MANAGEMENT_ADDRESS>:30080`, as in [Run NVKE end to end on one machine](../get-started/local-site.md), or the `https` address of the TLS endpoint you put in front of it. A token sent to the `http` address crosses the network unencrypted; see the warning at the top of this page. `nvkectl get kubeconfig`, `nvkectl get providers`, and the machine image, cluster release and node pool release commands, such as `nvkectl get nodepoolreleases`, read the management cluster's Kubernetes API directly with this kubeconfig. The bootstrap does not configure that API server to accept tokens from your IdP, so these commands fail. Use `kubectl` with `<CLUSTER_NAME>.kubeconfig` for them instead, as described in [Access a workload cluster](../workload-clusters/access.md#get-the-admin-kubeconfig) and [Create a workload cluster](../workload-clusters/create.md#before-you-begin).

3. Call the API.

   ```bash
   nvkectl get clusters
   ```

   `kubectl oidc-login` asks you to log in to the IdP if it has no valid token, then the command lists the workload clusters.

### With curl

1. Get a token.

   ```bash
   TOKEN="$(kubectl oidc-login get-token --oidc-issuer-url=<ISSUER_URL> --oidc-client-id=<CLIENT_ID> | jq -r .status.token)"
   ```

   Automation that gets its tokens another way works the same, as long as each token carries an accepted `aud` and an admitted group.

2. Call the API with the token in the `Authorization` header. `<API_URL>` is `http://<MANAGEMENT_ADDRESS>:30080`, or the `https` address of the TLS endpoint you put in front of it. Other pages write this header as `-H "Authorization: Bearer <TOKEN>"`, where `<TOKEN>` is a token like the one in `TOKEN`.

   ```bash
   curl -H "Authorization: Bearer ${TOKEN}" <API_URL>/api/clusters
   ```

   The response is the list of workload clusters in JSON.

## Find callers that enforcing mode would reject

Leave NVKE in permissive mode while your existing callers keep working, and watch which of them would fail.

1. Read the authentication counter.

   ```bash
   kubectl -n nvke-controller exec deploy/nvke-controller -- wget -qO- http://127.0.0.1:9090/metrics | grep '^nvke_auth_requests_total'
   ```

   ```text
   nvke_auth_requests_total{issuer="unknown",outcome="would_reject",reason="no_token"} 7
   ```

   Requests that pass are counted with `outcome="authenticated"`. Each `would_reject` series represents requests that enforcing mode would reject. Use the logs to trace the source requests.

2. Read the `reason` label and fix each caller.

   | `reason` | Status when enforcing | Fix |
   |---|---|---|
   | `no_token` | `401` | The caller sends no `Authorization: Bearer` header. Give it a token. |
   | `unknown_issuer` | `401` | The token's `iss` matches no configured `issuer` byte for byte. Fix issuer, or give the caller a token from your IdP. |
   | `audience_mismatch` | `401` | The token's `aud` is not in `audiences`. Add it, or give the caller a token for an accepted audience. |
   | `expired`, `not_yet_valid` | `401` | The token is outside its validity period. Get a new one, or check the caller's clock. |
   | `bad_signature`, `algorithm_not_allowed` | `401` | The token does not verify against the issuer's keys and `algorithms`. |
   | `jwks_stale` | `401` | NVKE cannot refresh the issuer's keys. Fix the key endpoint, not the caller. |
   | `not_admitted` | `403` | The token is valid, but its `groups` claim contains no admitted group. Add the caller to the group. |

3. Find the caller behind each rejection in the log.

   ```bash
   kubectl -n nvke-controller logs deploy/nvke-controller | grep 'authentication would have rejected this request'
   ```

   Each line names the `reason`, `issuer`, `method`, `path` and `remote_addr` of the request.

Continue when `would_reject` stops increasing.

## Switch to enforcing mode

1. In `auth-values.yaml`, set `permissive: false`, then run the install again with the same flags as in [Turn on authentication in permissive mode](#turn-on-authentication-in-permissive-mode), step 3.

   ```bash
   "$NVKE_REPO"/deploy/nvke-deploy-local.sh install \
     --values-for controller=../auth-values.yaml
   ```

2. Wait for the rollout.

   ```bash
   kubectl -n nvke-controller rollout status deploy/nvke-controller
   ```

   In enforcing mode NVKE does not start unless it fetches the signing keys of every configured issuer. If the new pod does not become ready, its log contains `Failed to set up API authentication` with the issuer and the error.

   In enforcing mode, NVKE logs each rejected request as `rejected request`, with the same fields as the permissive-mode message.

NVKE reads the authentication configuration once, at startup. If you later change only `apiAuth.config` or `apiAuth.configMap.extraFiles`, restart the Deployment with `kubectl -n nvke-controller rollout restart deploy/nvke-controller`. Add a new issuer in permissive mode first, because an issuer whose keys NVKE cannot fetch stops an enforcing NVKE from starting.

The deploy path's own values turn authentication off. Every time you run the install again, for any reason, add `--values-for controller=../auth-values.yaml`, along with the other inputs listed in [Run NVKE end to end on one machine](../get-started/local-site.md).

## Verify

1. Check the mode the controller reports.

   ```bash
   kubectl -n nvke-controller exec deploy/nvke-controller -- wget -qO- http://127.0.0.1:9090/metrics | grep '^nvke_auth_enabled'
   ```

   The output is `nvke_auth_enabled{mode="enforcing"} 1`.

2. Call the API without a token.

   ```bash
   curl -s -w '\n%{http_code}\n' <API_URL>/api/clusters
   ```

   The response is `401` with the message `Authentication required: no bearer token was presented.` A `200` means authentication is still off or permissive.

   Test with `/api/clusters`, not `/api/version`. `/healthz`, `/readyz`, `/api/version`, `/openapi.yaml`, `/docs` and the API documentation under `/api/docs/` need no token, so a successful call to them tells you nothing about authentication.

3. Call the API with a token from a user who is not in `<ADMITTED_GROUP>`. The response is `403` with the message `Not admitted: the authenticated identity matches no admission rule.`

## Next steps

- [Create a workload cluster](../workload-clusters/create.md).
