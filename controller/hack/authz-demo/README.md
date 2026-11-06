# Authorization demo

Role-based authorization of the NVKE controller API, on the local kind cluster
`kind-controller-capi`. Callers sign in to a local Keycloak, and optionally a
local Teleport. The controller maps each caller's group or Teleport role to an
NVKE role:

| Caller | Identity | NVKE role | Expected |
|---|---|---|---|
| alice | Keycloak group `nvke-admins` | `admin` | every operation |
| vera | Keycloak group `nvke-viewers` | `viewer` | every GET operation |
| nora | Keycloak group `nvke-users` | none | 403 from authorization |
| zed | no group | none | 403 `not_admitted` from authentication |
| ci-bot | Keycloak client, service account in `nvke-admins` | `admin` | every operation |
| ops-admin | Teleport role `nvke-admin` | `admin` | every operation |
| ops-viewer | Teleport role `nvke-viewer` | `viewer` | every GET operation |
| ops-nobody | Teleport role `nvke-none` | none | 403 `not_admitted` |

Demo only: every password, the `ci-bot` secret and the Keycloak admin login
(`admin`/`admin`) are committed on purpose. Generated keys and certificates go
to `controller/.local/authz-demo`, which git ignores.


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

Run these from `controller/`. Without Teleport, skip step 1 and drop the
Teleport flags.

1. Teleport only: build the image and load it into kind. The first build takes
   about 3.5 minutes.

   ```bash
   hack/authz-demo/teleport-image/build.sh
   kind load docker-image nvke-demo/teleport:18.11.1-agpl --name controller-capi
   ```

2. Start Tilt with the demo on.

   ```bash
   NVKE_AUTHZ_DEMO=true NVKE_AUTHZ_DEMO_TELEPORT=true tilt up
   ```

   Tilt runs `gen.sh`, deploys Keycloak (and Teleport) before the controller,
   forwards Keycloak to `localhost:18443` and Teleport to `localhost:3080`, and
   installs the controller with `apiAuthz.enabled: true` and the values in
   `.local/authz-demo/values.yaml` (`values-teleport.yaml` with Teleport).

3. Set up the take, and wait for `ready`.

   ```bash
   hack/authz-demo/demo.sh --setup --with-teleport
   ```

4. Run the whole walkthrough unattended.

   ```bash
   hack/authz-demo/demo.sh --with-teleport
   ```

## What demo.sh checks

`demo.sh` prints every command it runs, then `PASS` or `FAIL` per check.

| Section | Shows | Expected |
|---|---|---|
| 1 | alice lists clusters | 200 |
| 2 | vera lists, then deletes a cluster | 200, then 403 `Not authorized`, and a `refused request` log line naming vera's subject and `deleteCluster` |
| 3 | nora (admitted, no role) and zed (not admitted) list clusters | 403 `Not authorized` from authorization; 403 `Not admitted` from authentication, logged as `rejected request` with `"reason":"not_admitted"` |
| 4 | a token signed by an untrusted issuer | 401 `unknown_issuer` |
| 5 | expired, malformed and tampered tokens | 401 `expired`, then `bad_signature` twice; no token or signature in the controller's log |
| 6 | every operation in `GET /openapi.yaml`, with no token, as vera and as alice | 401 with no token; vera: every GET answered by its handler, every write 403; alice: every operation answered by its handler |
| 7 | ci-bot, a client-credentials service client | 200 |
| 8 | ops-admin, ops-viewer and ops-nobody through the Teleport app | 200; 403 `Not authorized` logged with subject `ops-viewer` and issuer `teleport`; 403 `Not admitted` |
| 9 | `nvke_authz_enabled` and `nvke_authz_decisions_total` | `mode="enforcing"`, and `deleteCluster` denials counted |

Section 6 creates nothing. Path IDs are random UUIDs, so a handler 404 shows
the request got past authorization. `createCluster` names a provider the
deployment doesn't support, and every other write names a resource that
doesn't exist.

The expired token is signed with the realm's own key, so the controller
verifies it and refuses it for its `exp`. Teleport puts its own ES256 JWT in
the `Authorization` header: its `iss` is `teleport.nvke-demo.local`, its `aud`
the app's `uri`, and its `roles` claim the user's Teleport roles.

Run `demo.sh --setup --with-teleport` before every take and after the talk.
It restarts Keycloak, deletes what a take wrote under `.local/authz-demo/take`
and the certificates in the Teleport pod, applies the Teleport roles and
users, waits for the `nvke` app to register, and prints `ready` once alice can
list clusters.
