# Lab Keycloak

Local OpenID Connect issuer for NVKE API and tenant-kubeconfig work. Lab only:
the admin password, client secret, and test user are committed on purpose.

The issuer hostname is `keycloak.nke.test`, not `localhost`. Tokens always
carry `iss=http://keycloak.nke.test:8180/realms/nke`. The laptop reaches that
name through `/etc/hosts` → `127.0.0.1`. Kind reaches the same name through
CoreDNS and node `/etc/hosts` → the container address on the `kind` Docker
network. Port 8180 avoids Tilt's CWE forward on `localhost:8080`. The admin
console still serves on `http://localhost:8180`.

## Run

Not started by Tilt or `make capd-up`. Bring Kind up first, then this sidecar.

```bash
# from controller/
make capd-up

# once on the laptop, so it uses the same host as Kind
sudo sh -c 'grep -q keycloak.nke.test /etc/hosts || echo "127.0.0.1 keycloak.nke.test" >> /etc/hosts'

# from this directory
./up.sh          # start Keycloak, attach to kind, patch node hosts + CoreDNS
./up.sh --verify # also mint a password-grant token and probe from a Kind pod
./down.sh
```

`up.sh` defaults to Kind cluster `cwe-capi` (context `kind-cwe-capi`). Override
with `KIND_CLUSTER_NAME` / `KUBE_CONTEXT`. Re-run `./up.sh` after `kind create`
or `make capd-up` if Keycloak started first — it will then wire the cluster.

## Identities

| | |
|---|---|
| Admin console | `http://localhost:8180` — `admin` / `admin` |
| Issuer | `http://keycloak.nke.test:8180/realms/nke` |
| Audience | `https://nke.tilt.local` |
| Group | `nke-operators` |
| User | `nke-dev` / `nke-dev` |
| Public client | `nke-cli` (authorization code, device grant, localhost redirects, password grant) |
| Static client | `nke-static` / `nke-static-lab-secret` (password and client-credentials grants) |

Mint a token without a browser:

```bash
curl -fsS --resolve keycloak.nke.test:8180:127.0.0.1 \
  -d grant_type=password \
  -d client_id=nke-static \
  -d client_secret=nke-static-lab-secret \
  -d username=nke-dev \
  -d password=nke-dev \
  -d scope=openid \
  http://keycloak.nke.test:8180/realms/nke/protocol/openid-connect/token
```
