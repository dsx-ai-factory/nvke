# nico-prereqs

Step 3 of the [sequence](../README.md#the-sequence): everything NICo expects
to find already in place.

```bash
ssh-keygen -t ed25519 -f ./ssh_host_ed25519_key -N ""

helm upgrade --install nico-prereqs deploy/charts/nico-prereqs \
  --namespace forge-system --create-namespace \
  --values deploy/charts/nico-prereqs/values.yaml \
  --set database.password=<pick one> \
  --set-file sshHostKey.privateKey=./ssh_host_ed25519_key \
  --wait
```

Then [`../nico`](../nico/) into the same namespace.

## What it creates, and why each one is here

| | For |
|---|---|
| The namespace, via `--create-namespace` | NICo's hooks are namespaced |
| PostgreSQL | The migration Job connects to it |
| `nico.forge-pg-cluster.credentials` | The Job reads `DATASTORE_USER` and `DATASTORE_PASSWORD` from it |
| `nico-vault-approle-tokens`, `nico-vault-token` | Mounted by NICo's services whether or not a Vault exists |
| `ssh-host-key` | `nico-ssh-console-rs` |
| A `vault-nico-issuer` ClusterIssuer | Every NICo service certificate |
| Its own Temporal, optionally | NICo's workflows, in NICo's namespace |

The first three are the reason this is a separate release rather than part of
[`../nico`](../nico/). Helm applies hooks before every regular manifest in
their own release, so anything NICo's hooks read has to exist beforehand. See
[`../nico/README.md`](../nico/README.md#why-the-prerequisites-are-a-separate-release).

## Replacing Vault PKI

NICo signs its service certificates through a cert-manager `ClusterIssuer`
backed by a Vault PKI engine, and expects it to be called `vault-nico-issuer`.

With a Vault, leave `certManager.issuer.create` false and point
`certManager.issuer.name` at the ClusterIssuer that Vault backs. Without one,
set it true and this chart creates a self-signed CA chain under the same name.
NICo cannot tell the difference: it only ever names the issuer.

**This is not a substitute for Vault PKI on a real site.** No revocation, no
audit trail, no rotation story, and the CA private key is a Secret in this
cluster.

## The SSH host key is supplied, never generated

`nico-ssh-console-rs` presents it to every client that opens a machine
console. Helm would mint a new key on every upgrade, and every client holding
the old one would report a host-key mismatch. Generate it once, keep it, and
pass it with `--set-file`. The Secret carries `helm.sh/resource-policy: keep`
for the same reason.

## NICo's Temporal

`temporal.enabled` is off by default. When on, it lands in this release's
namespace — NICo's — which is separate from the NVKE controller's by
construction, because they are separate releases. Its Services are named
`nico-temporal-*` either way.

## Two values that must agree with `../nico`

Nothing checks the first of these for you:

| Here | There |
|---|---|
| `database.host` | `nico.nico-api.databaseConfig.DB_HOST` |
| `database.secretName` | `nico.nico-api.envFrom.databaseCredentials.secretName` |

The second is checked: `../nico/templates/validate.yaml` refuses the install
when the Secret it names is not in the namespace.
