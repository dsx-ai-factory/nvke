# Authorization demo

Role-based authorization of the NVKE controller API, on the local kind cluster
`kind-controller-capi`. Callers sign in to a local Keycloak, and optionally a
local Teleport. The controller maps each caller's group or Teleport role to an
NVKE role:

| Caller | Identity | NVKE role | Expected |
|---|---|---|---|
| alice | Keycloak group `nvke-admins` | `admin` | every operation |
| vera | Keycloak group `nvke-viewers` | `viewer` | the 13 GET operations |
| nora | Keycloak group `nvke-users` | none | 403 from authorization |
| zed | no group | none | 403 `not_admitted` from authentication |
| ci-bot | Keycloak client, service account in `nvke-admins` | `admin` | every operation |
| ops-admin | Teleport role `nvke-admin` | `admin` | every operation |
| ops-viewer | Teleport role `nvke-viewer` | `viewer` | the GET operations |
| ops-nobody | Teleport role `nvke-none` | none | 403 `not_admitted` |

Demo only: every password, the `ci-bot` secret and the Keycloak admin login
(`admin`/`admin`) are committed on purpose. Generated keys and certificates go
to `controller/.local/authz-demo`, which git ignores.

Rehearsed on kind on 2026-11-05: every check passes. `--setup` takes 16 to 35 s; the walkthrough takes about 4 s.

## Files

| File | What |
|---|---|
| `gen.sh` | The demo CA, serving certificates, the realm's signing key, a rogue key, and the controller values |
| `keycloak.yaml`, `realm-nvke.json` | Keycloak `26.8.2` in namespace `nvke-authz-demo`, and the `nvke` realm it imports |
| `teleport.yaml`, `teleport-resources.yaml` | Teleport `18.11.1` (auth, proxy and the `nvke` app in one pod), and its roles and users |
| `teleport-image/` | Builds the Teleport image from the AGPL source |
| `mint/` | Makes the rogue, expired, tampered and malformed tokens |
| `demo.sh` | The scripted walkthrough, and `--setup` |

## Bring up

Run these from `controller/`.

1. Teleport only: build the image and load it into kind. The first build takes
   about 3.5 minutes.

   ```bash
   hack/authz-demo/teleport-image/build.sh
   kind load docker-image nvke-demo/teleport:18.11.1-agpl --name controller-capi
   ```

2. Start Tilt with the demo on. Without Teleport, leave out
   `NVKE_AUTHZ_DEMO_TELEPORT=true`.

   ```bash
   NVKE_AUTHZ_DEMO=true NVKE_AUTHZ_DEMO_TELEPORT=true tilt up
   ```

   Tilt runs `gen.sh`, deploys Keycloak (and Teleport) before the controller,
   forwards Keycloak to `localhost:18443` and Teleport to `localhost:3080`, and
   installs the controller with `apiAuthz.enabled: true` and the values in
   `.local/authz-demo/values.yaml` (`values-teleport.yaml` with Teleport).

3. Set up the take, and wait for `ready`. Without Teleport, leave out
   `--with-teleport`.

   ```bash
   hack/authz-demo/demo.sh --setup --with-teleport
   ```

4. Run the whole walkthrough unattended.

   ```bash
   hack/authz-demo/demo.sh --with-teleport
   ```

   It prints `PASS` or `FAIL` per check and exits 0 when all pass, 1 when one
   fails, and 2 when a preflight check fails.

## Steps

Each block runs on its own from the repository root.

### 1. An admin lists clusters

```bash
cd controller/.local/authz-demo
KC=https://localhost:18443/realms/nvke
ALICE=$(curl -sS --cacert ca.crt -d grant_type=password -d client_id=nvkectl -d username=alice -d password=alice-demo "$KC/protocol/openid-connect/token" | jq -r .access_token)
curl -s -w '\n%{http_code}\n' -H "Authorization: Bearer $ALICE" http://localhost:8080/api/clusters
```

Expect 200.

### 2. A viewer reads but can't delete

```bash
cd controller/.local/authz-demo
KC=https://localhost:18443/realms/nvke
START=$(date -u +%Y-%m-%dT%H:%M:%SZ)
VERA=$(curl -sS --cacert ca.crt -d grant_type=password -d client_id=nvkectl -d username=vera -d password=vera-demo "$KC/protocol/openid-connect/token" | jq -r .access_token)
curl -s -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $VERA" http://localhost:8080/api/clusters
curl -s -w '\n%{http_code}\n' -H "Authorization: Bearer $VERA" -X DELETE "http://localhost:8080/api/clusters/$(uuidgen | tr A-Z a-z)"
kubectl --context kind-controller-capi -n nvke-controller logs deploy/nvke-controller -c nvke-controller --since-time=$START | jq -cR 'fromjson? | select(.msg == "refused request") | {action, subject, issuer, resource_type, resource_id}'
```

Expect 200, then 403 `{"message":"Not authorized"}`, then a log line with
`"action":"deleteCluster"` and vera's subject, `00000000-0000-4000-8000-00000000fe1a`.

### 3. Admitted without a role, and not admitted

```bash
cd controller/.local/authz-demo
KC=https://localhost:18443/realms/nvke
NORA=$(curl -sS --cacert ca.crt -d grant_type=password -d client_id=nvkectl -d username=nora -d password=nora-demo "$KC/protocol/openid-connect/token" | jq -r .access_token)
ZED=$(curl -sS --cacert ca.crt -d grant_type=password -d client_id=nvkectl -d username=zed -d password=zed-demo "$KC/protocol/openid-connect/token" | jq -r .access_token)
curl -s -w '\n%{http_code}\n' -H "Authorization: Bearer $NORA" http://localhost:8080/api/clusters
curl -s -w '\n%{http_code}\n' -H "Authorization: Bearer $ZED" http://localhost:8080/api/clusters
```

Expect 403 `Not authorized` for nora, from authorization, and 403 `Not admitted:
the authenticated identity matches no admission rule.` for zed, from
authentication. The controller logs nora as `refused request` and zed as
`rejected request` with `"reason":"not_admitted"`.

### 4. A token from an untrusted issuer

```bash
cd controller/.local/authz-demo
ROGUE=$(./mint rogue --key rogue-key.pem --iss https://rogue.nvke-demo.local --sub mallory --groups nvke-admins)
curl -s -w '\n%{http_code}\n' -H "Authorization: Bearer $ROGUE" http://localhost:8080/api/clusters
```

Expect 401. `demo.sh --setup` builds `./mint`.

### 5. Expired, malformed and tampered tokens

```bash
cd controller/.local/authz-demo
KC=https://localhost:18443/realms/nvke
curl -sS --cacert ca.crt "$KC/protocol/openid-connect/certs" -o take/certs.json
EXPIRED=$(./mint sign --key realm-key.pem --jwks take/certs.json --iss https://keycloak.nvke-authz-demo.svc/realms/nvke --sub alice --groups nvke-admins --ttl -10m)
VERA=$(curl -sS --cacert ca.crt -d grant_type=password -d client_id=nvkectl -d username=vera -d password=vera-demo "$KC/protocol/openid-connect/token" | jq -r .access_token)
TAMPERED=$(./mint tamper --token "$VERA" --claim 'groups=["nvke-admins"]')
for t in "$EXPIRED" "$(./mint malformed)" "$TAMPERED"; do curl -s -w '\n%{http_code}\n' -H "Authorization: Bearer $t" http://localhost:8080/api/clusters; done
```

Expect 401 three times: `expired`, then `bad_signature` twice. The expired token
is signed with the realm's own key, so the controller verifies it and refuses
it for its `exp`. `demo.sh` also checks that no token or signature reaches the
controller's log.

### 6. Every operation

```bash
controller/hack/authz-demo/demo.sh
```

Section 6 of the output. The script reads `GET /openapi.yaml` and calls every operation with no token,
as vera, and as alice. Path IDs are random UUIDs, so a handler 404 shows the
request got past authorization. Expect 401 with no token; for vera, every GET
answered by its handler and every write 403; for alice, every operation
answered by its handler. No call creates or deletes anything: `createCluster`
names a provider the deployment doesn't support, and every other write names a
resource that doesn't exist.

### 7. A service client

```bash
cd controller/.local/authz-demo
KC=https://localhost:18443/realms/nvke
CI=$(curl -sS --cacert ca.crt -d grant_type=client_credentials -d client_id=ci-bot -d client_secret=ci-bot-demo-secret "$KC/protocol/openid-connect/token" | jq -r .access_token)
curl -s -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $CI" http://localhost:8080/api/clusters
```

Expect 200.

### 8. Teleport

```bash
cd controller/.local/authz-demo
for u in ops-admin ops-viewer ops-nobody; do
  kubectl --context kind-controller-capi -n nvke-authz-demo exec deploy/teleport -- tctl auth sign --user $u --app-name nvke --format tls --ttl 1h --overwrite -o /tmp/$u >/dev/null
  for f in crt key; do kubectl --context kind-controller-capi -n nvke-authz-demo exec deploy/teleport -- cat /tmp/$u.$f > take/$u.$f; done
done
TP="--cacert ca.crt --resolve nvke.teleport.nvke-demo.local:3080:127.0.0.1"
curl -s -o /dev/null -w '%{http_code}\n' $TP --cert take/ops-admin.crt --key take/ops-admin.key https://nvke.teleport.nvke-demo.local:3080/api/clusters
curl -s -w '\n%{http_code}\n' $TP --cert take/ops-viewer.crt --key take/ops-viewer.key -X DELETE "https://nvke.teleport.nvke-demo.local:3080/api/clusters/$(uuidgen | tr A-Z a-z)"
curl -s -w '\n%{http_code}\n' $TP --cert take/ops-nobody.crt --key take/ops-nobody.key https://nvke.teleport.nvke-demo.local:3080/api/clusters
```

Expect 200, 403 `Not authorized` with a `refused request` log line naming
subject `ops-viewer` and issuer `teleport`, and 403 `Not admitted`. Teleport
puts its own ES256 JWT in the `Authorization` header; its `iss` is
`teleport.nvke-demo.local`, its `aud` the app's `uri`, and its `roles` claim
the user's Teleport roles.

### 9. Decision metrics

```bash
kubectl --context kind-controller-capi get --raw /api/v1/namespaces/nvke-controller/services/nvke-controller:metrics/proxy/metrics | grep -E '^nvke_authz_(decisions_total|enabled|bundle_reload_failures_total)'
```

Expect `nvke_authz_enabled{mode="enforcing"} 1` and
`nvke_authz_decisions_total` counts by `action` and `outcome`.

### 10. Reset

```bash
controller/hack/authz-demo/demo.sh --setup --with-teleport
```

Run it before every take and after the talk. It restarts Keycloak, deletes
what a take wrote under `.local/authz-demo/take` and the certificates in the
Teleport pod, applies the Teleport roles and users, waits for the `nvke` app to
register, and prints `ready` once alice can list clusters.
