# Configure OIDC for workload clusters

This page describes how to make the Kubernetes API servers of the workload clusters that NVIDIA Kubernetes Engine (NVKE) creates accept JSON Web Tokens (JWTs) from OpenID Connect (OIDC) identity providers (IdPs). Do it after [Run NVKE end to end on one machine](../get-started/local-site.md) and before you create the workload clusters that need it: a workload cluster keeps the fleet-wide providers it was created with.

A workload cluster's API server trusts two kinds of provider:

| | Shared provider | Fleet-wide providers |
|---|---|---|
| Chart values | `nvkeOIDCAuth` | `oidc.providers` |
| Tokens it accepts | SPIFFE JWTs that the NVKE management plane and the Cluster API controllers use to reach the cluster | Tokens from IdPs you run, for people and automation |
| Audience | The cluster name | The `audiences` you list |
| Username | The `sub` claim, with no prefix | The claim and prefix you choose |
| Groups | None from the token | The claim and prefix you choose |
| Number | One | Up to five |
| Which clusters have it | Clusters created while `nvkeOIDCAuth.enabled` is `true`, and clusters you turn it on for with `enableOidc` | Clusters created on Kubernetes 1.34 or later while the provider is listed |

NVKE binds Kubernetes roles only to the management plane's own identities. A fleet-wide provider authenticates users and grants them nothing: bind roles to its usernames and groups in each cluster with the admin kubeconfig, as described in [Access a workload cluster](access.md#what-the-customer-manages).

## Before you begin

- You have completed [Install the management plane](../operate/install-management-plane.md). You run the install and `kubectl` commands on the host you installed from, from the root of the same `nvke/` checkout, with the site spec, `<CLUSTER_NAME>.kubeconfig` and, if you installed from one, `release.json` in the directory above it. Set `KUBECONFIG` for `kubectl`:

  ```bash
  export KUBECONFIG="$PWD/../<CLUSTER_NAME>.kubeconfig"
  ```

- The shell you run the install from has the `NVKECTL_*` variables from [Run NVKE end to end on one machine](../get-started/local-site.md) exported, as described in [Run NVKE end to end on one machine](../get-started/local-site.md).
- For each fleet-wide provider you have its issuer URL, the audience its tokens carry, the claims that hold the username and groups, and the certificate authority (CA) certificates that sign its HTTPS certificate. [Collect the identity provider values](../operate/api-authentication.md#collect-the-identity-provider-values) shows how to read the first three from the IdP's discovery document and a token.
- The control-plane nodes of your workload clusters reach each provider's discovery document, and the `jwks_uri` it names, over HTTPS. NVKE does not check this.
- The workload clusters that need fleet-wide providers run Kubernetes 1.34 or later. [Supported platforms and versions](../overview/supported-platforms.md) lists the version this release supports.

## Add fleet-wide providers

1. Write the providers to a values file named `oidc-values.yaml`, in the directory above the `nvke/` checkout.

   ```yaml
   oidc:
     providers:
       - name: example-idp
         issuerURL: https://idp.example.com/realms/example
         audiences: ["<AUDIENCE>"]
         usernameClaim: email
         usernamePrefix: "example-idp:"
         groupsClaim: groups
         groupsPrefix: "example-idp:"
         caBundle: |
           -----BEGIN CERTIFICATE-----
           ...
           -----END CERTIFICATE-----
   ```

   `<AUDIENCE>` is the `aud` value of the provider's tokens. Each provider takes these fields:

   | Field | Required | Value |
   |---|---|---|
   | `name` | Yes | A label, unique among the providers. Error messages name the provider by it. |
   | `issuerURL` | Yes | The issuer, compared with each token's `iss` claim. It must use `https`. Unique among the providers, and different from `nvkeOIDCAuth.issuerURL` while the shared provider is on. |
   | `audiences` | Yes | The `aud` values the API server accepts. A token is accepted if it names any of them. |
   | `usernameClaim` | Yes | The claim that holds the username, such as `email` or `sub`. |
   | `usernamePrefix` | No | Text put in front of every username. Empty adds nothing. |
   | `groupsClaim` | No | The claim that holds the user's groups. Without it, the provider's users get no groups from the token. |
   | `groupsPrefix` | No | Text put in front of every group. It applies only with `groupsClaim`. |
   | `requiredClaims` | No | A map of claim names to the value each must hold. A token without one of the values is rejected. |
   | `caBundle` | Yes | The PEM CA certificates that the API server trusts when it fetches the provider's discovery document and signing keys. It trusts no other CA for the provider, so include the root CA even when a public CA signs the provider's certificate. |
   | `discoveryURL` | No | Where the API server fetches the discovery document when it cannot reach it under `issuerURL`. An absolute `https` URL with no username, password, query or fragment, different from `issuerURL`, and unique among the providers and the shared provider. |

   `usernamePrefix` and `groupsPrefix` must not start with `system:`, `kubeadm:`, `nke:`, `nvke:` or `nvidia:`. Give each provider a different prefix, so that its users and groups cannot collide with another provider's. See [Reserved identities](#reserved-identities) for what the API server refuses regardless of the prefix.

2. Run the install command from [Run NVKE end to end on one machine](../get-started/local-site.md) again, with the same flags and `--values-for controller=../oidc-values.yaml` added.

   ```bash
   "$NVKE_REPO"/deploy/nvke-deploy-local.sh install \
     --values-for controller=../oidc-values.yaml
   ```

   The wrapper reads its inputs from the `NVKE_*` variables that `env.sh` exports. If you passed other values files before, such as `../auth-values.yaml` from [Secure the NVKE API](../operate/api-authentication.md) or a values file for NVKE telemetry pipeline (NKTP) with `--values-for nktp=`, pass each of them again with its own `--values-for` flag: a file you leave out stops applying, and the deploy path's own values turn API authentication off and send telemetry to the bundled development backend. The chart refuses to render more than five providers.

3. Restart the controller so that it reads the new configuration.

   ```bash
   kubectl -n nvke-controller rollout restart deploy/nvke-controller
   ```

   ```bash
   kubectl -n nvke-controller rollout status deploy/nvke-controller --timeout 5m
   ```

   The output ends with `successfully rolled out`. The controller reads its configuration only when it starts, and the install does not restart it when only the configuration changes. If the rollout does not finish, the controller has refused the configuration and the NVKE API is unavailable until you fix it; see [If the controller does not start](#if-the-controller-does-not-start).

4. Create the workload clusters that need the providers, as described in [Create a workload cluster](create.md). Clusters that already exist keep the providers they were created with.

## Turn on the shared provider

The install configures the shared provider for the management cluster's SPIRE server and leaves it off. `nvkeOIDCAuth.issuerURL` is `https://nke.site-controller.internal`, the issuer of SPIRE's JWTs, and `nvkeOIDCAuth.discoveryURL` is `https://<MANAGEMENT_ADDRESS>:30443/.well-known/openid-configuration`, where `<MANAGEMENT_ADDRESS>` is the management cluster's virtual IP address (VIP). The API server trusts the CA in the `oidc-provider-upstream-ca` Secret in the `spire-server` namespace.

1. Add the shared provider to `oidc-values.yaml`.

   ```yaml
   nvkeOIDCAuth:
     enabled: true
   ```

2. Run the install command and restart the controller, as in steps 2 and 3 of [Add fleet-wide providers](#add-fleet-wide-providers).

   Every workload cluster you create from now on has the shared provider. Leave `nvkeOIDCAuth.enabled` set to `true` afterward: the chart grants the controller read access to the CA Secret only while it is set, and the controller reads that Secret whenever a control-plane node joins a cluster that has the shared provider.

3. To turn the shared provider on for a workload cluster that already exists, patch the cluster with `enableOidc: true`, as described in [Change cluster settings after creation](create.md#change-cluster-settings-after-creation).

   ```bash
   nvkectl patch cluster <WORKLOAD_CLUSTER> --patch 'enableOidc: true'
   ```

   The change cannot be undone. NVKE updates one control-plane node at a time, and each node's API server must report ready before the next node starts. The rollout requires a cluster name that is a valid DNS subdomain, because the name is the token audience, and node agents on every control-plane node that support the `PatchAPIServerManifest` remote procedure call (RPC).

## Verify

1. Confirm that a workload cluster created after step 3 of [Add fleet-wide providers](#add-fleet-wide-providers) accepts a token from a fleet-wide provider. Get an ID token for a test user from the provider, for example with `kubectl oidc-login get-token` as in [Secure the NVKE API](../operate/api-authentication.md), and save the cluster's CA certificate from its admin kubeconfig.

   ```bash
   kubectl --kubeconfig <WORKLOAD_CLUSTER>.kubeconfig config view --raw --minify -o jsonpath='{.clusters[0].cluster.certificate-authority-data}' | base64 -d > <WORKLOAD_CLUSTER>-ca.crt
   ```

   ```bash
   kubectl --kubeconfig /dev/null --server https://<API_ADDRESS>:6443 --certificate-authority <WORKLOAD_CLUSTER>-ca.crt --token "<ID_TOKEN>" auth whoami
   ```

   `<API_ADDRESS>` is the address from [Access a workload cluster](access.md), and `<ID_TOKEN>` is the token. The output shows the username as `usernamePrefix` followed by the value of `usernameClaim`, such as `example-idp:alice@example.com`, and the groups from `groupsClaim`, each with `groupsPrefix`, followed by `system:authenticated`. `error: You must be logged in to the server (Unauthorized)` means the API server rejected the token; see [If a token is rejected](#if-a-token-is-rejected).

2. Confirm that a cluster has the shared provider. On the management cluster, read the mode label of the cluster's management-plane kubeconfig.

   ```bash
   kubectl -n nvke-system get secret <WORKLOAD_CLUSTER>-kubeconfig -o jsonpath='{.metadata.labels.nvke\.nvidia\.com/kubeconfig-mode}'
   ```

   The output is `spire` when the management plane reaches the cluster with SPIFFE JWTs through the shared provider, and `admin` when it uses the admin kubeconfig. NVKE switches it to `spire` after it applies its own role bindings in the cluster. If it stays `admin` after the cluster is `Provisioned` or the rollout succeeds, the controller's log says why: NVKE could not read the RBAC ConfigMap in `nvke-system` — `nvke-rbac` by default, `nke-rbac` on a site that pinned the old name — or could not apply the role bindings. The log names the namespace and ConfigMap it looked for.

## How NVKE applies the providers

The chart renders both kinds of provider into `nvke-controller-config` (`nke-controller-config` on an install that pins the old name), the ConfigMap in the `nvke-controller` namespace that holds the controller's settings: `nvkeOIDCAuth` becomes the `oidc.managementOIDCAuth` key, with `caBundleSecret` renamed `caBundleSecretRef`, and `oidc.providers` keeps its name. The controller reads the file once, when it starts, and refuses to start when the file is invalid. It takes these settings only from the file: it ignores the `NKE_OIDC_AUTH_*` environment variables, and the chart refuses to render an `env` entry that sets one.

NVKE writes a workload cluster's providers into `/etc/kubernetes/oidc/auth-config.yaml` on each control-plane node and passes it to the API server with `--authentication-config`. The two kinds of provider reach the file differently:

- **Fleet-wide providers are fixed when the cluster is created.** NVKE copies `oidc.providers` into the cluster's record, and `kubeadm init`, every later control-plane join and the `enableOidc` rollout build the file from that copy, so every control-plane node has the same providers. Changing `oidc.providers` affects only clusters created afterward. NVKE has no procedure to add, change or remove a fleet-wide provider on an existing cluster.
- **A cluster created on Kubernetes earlier than 1.34 gets no fleet-wide providers.** Its API server cannot read the configuration NVKE renders. The controller logs `Creating cluster without the fleet-wide OIDC providers`, and upgrading the cluster later does not add them.
- **The shared provider's settings are read when a node joins.** Whether a cluster has the shared provider is fixed at creation or by `enableOidc`. Its issuer URL, discovery URL and CA bundle come from the controller's current configuration each time a control-plane node joins, so a changed setting reaches new and replaced control-plane nodes but not existing ones. Do not change them while workload clusters with the shared provider exist.

### Reserved identities

For each fleet-wide provider, NVKE adds Common Expression Language (CEL) validation rules to the API server's configuration. The API server evaluates them after it maps the token's claims, and rejects a token whose username or any of whose groups:

- starts with `system:`, `kubeadm:`, `nke:`, `nvke:` or `nvidia:`;
- starts with `spiffe://<TRUST_DOMAIN>/`, where `<TRUST_DOMAIN>` is the management plane's SPIFFE trust domain;
- equals a user that NVKE's own role bindings name, such as the management plane's SPIFFE ID.

A fleet-wide provider therefore cannot sign in a user as a Kubernetes or kubeadm identity, such as a member of the `kubeadm:cluster-admins` group, or as the management plane, whatever its tokens claim. The shared provider has no such rules, because it is how the management plane authenticates.

## If the controller does not start

Read the controller's log.

```bash
kubectl -n nvke-controller logs deploy/nvke-controller
```

The error begins `Error loading configuration: invalid controller config` and names the provider and the rule it breaks, for example `oidc: providers[0] (example-idp): usernamePrefix "system:" is inside the reserved "system:" namespace`. Fix `oidc-values.yaml`, then repeat steps 2 and 3 of [Add fleet-wide providers](#add-fleet-wide-providers).

## If a token is rejected

The API server rejects a fleet-wide provider's token when:

- the cluster was created before you added the provider, or on Kubernetes earlier than 1.34;
- the token's `iss` claim does not equal `issuerURL`, or its `aud` claim names none of `audiences`;
- the token lacks `usernameClaim`, or a claim in `requiredClaims` does not hold the required value;
- the API server cannot fetch the discovery document or the signing keys, because the network blocks it or `caBundle` does not include the CA that signs the provider's certificate;
- the mapped username or a group is [reserved](#reserved-identities). The API server logs `username uses a reserved prefix` or `groups use a reserved prefix`.

On a control-plane node, `/etc/kubernetes/oidc/auth-config.yaml` shows the providers that node's API server was given.

## If turning on the shared provider fails

When an `enableOidc` rollout fails, `nvkectl get clusters <WORKLOAD_CLUSTER>` shows `STATUS` `UpdateFailed`, and the cause is in the controller's log. A failure can leave some API servers with the shared provider:

- When a node fails to apply the change, NVKE tries to undo it on the nodes it already updated, restoring each node's previous configuration. The undo is best-effort.
- When a node fails its readiness check before it is changed, the rollout stops and the nodes already updated keep the change.
- When every node was updated and a later step fails, nothing is undone.

To retry, send the same patch again. Nodes that already have the change are left as they are.

## Next steps

- [Access a workload cluster](access.md), to give the customer the admin kubeconfig they bind roles with.
- [Troubleshoot](../operate/troubleshoot.md).
