# Authorize API callers by role

This page describes how to limit what each caller of the NVIDIA Kubernetes Engine (NVKE) API can do, by giving it a role. Do this after [Secure the NVKE API](api-authentication.md): authorization decides what an admitted caller may do, so it needs authentication on.

Authorization checks every API operation before NVKE serves it, by the operation's `operationId` in the OpenAPI spec, such as `listClusters` or `deleteCluster`. The paths that need no token, such as `/healthz` and `/api/version`, are not operations and are not checked. A role applies to every workload cluster: NVKE has no per-cluster permissions.

| Mode | Behavior |
|---|---|
| Off | Every admitted caller can call every operation. This is the default. |
| Permissive | Every request is checked, counted and logged, then served. Nothing is refused. |
| Enforcing | An admitted caller whose roles do not allow the operation gets `403` with `{"message":"Not authorized"}`. |

A token that authentication rejects still gets `401`, or `403` with `Not admitted: ...`, before authorization runs.

## Before you begin

- You have completed [Secure the NVKE API](api-authentication.md), and its Helm values are in `auth-values.yaml`, in the directory above the `nvke/` checkout. Authentication may still be permissive.
- You run the install and `kubectl` commands as in that page, with `KUBECONFIG` set to the management cluster's admin kubeconfig.
- You know which claim value, or which service account, identifies each caller of the API.

## Roles

The chart ships two roles:

- **`admin`**: may call every operation.
- **`viewer`**: may call the `get` and `list` operations, such as `getCluster` and `listNodePools`.

A caller with no role is refused every operation. A caller holds every role that any of its grants gives.

To change what a role allows, or to add a role, set `apiAuthz.policies` (Cedar policies) and `apiAuthz.roles` (the role list) in the values. The defaults are in the chart's `values.yaml`. A policy names actions as `NVKE::Action::"<operationId>"`, and NVKE refuses to start with a policy that names an action the API does not have.

## Grant roles

Grants live under `apiAuthz.config.issuers`, keyed by the `name` of an issuer in `apiAuth.config.issuers`. Each issuer takes two kinds of grant:

- **`claims`**: a token whose top-level claim `name` is the string `value`, or a string array holding it, gets the roles in `grants`. This is the same match as `admit.claims` in authentication. Use a claim the identity provider (IdP) sets, not one users can edit.
- **`serviceAccounts`**: a Kubernetes service account token from `namespace`, with `name` if it is set, gets the roles in `grants`.

For the IdP from [Secure the NVKE API](api-authentication.md), which admits by group:

```yaml
apiAuthz:
  enabled: true
  permissive: true
  config:
    issuers:
      example-idp:
        claims:
          - {name: groups, value: <ADMITTED_GROUP>, grants: [admin]}
          - {name: groups, value: <VIEWER_GROUP>, grants: [viewer]}
```

Once authorization enforces, every caller that authentication admits needs a role, including automation. If management-engine calls the API, admit its service account in `apiAuth` and grant it `admin`, under the issuer that admits it:

```yaml
      local-kubernetes:
        serviceAccounts:
          - {namespace: management-engine-system, name: management-engine-controller-manager, grants: [admin]}
```

NVKE checks the grants at startup and on every reload. It refuses an issuer that `apiAuth` does not configure, a grant of a role that `apiAuthz.roles` does not declare, and the tenancy keys `operatorTenant`, `tenant` and `operators`, which this API does not use.

### Teleport application access

Teleport signs a JSON Web Token (JWT) for each request to an application, with the user's Teleport roles in its `roles` claim, and has no `groups` claim. Admit those tokens by role in `apiAuth`, and grant NVKE roles by the same claim:

```yaml
apiAuth:
  config:
    issuers:
      - name: teleport
        issuer: "<TELEPORT_CLUSTER_NAME>"
        audiences: ["<APP_URI>"]
        jwks:
          uri: "https://<TELEPORT_PROXY_ADDRESS>/.well-known/jwks.json"
        algorithms: [RS256, ES256]
        admit:
          claims:
            - {name: roles, value: nvke-admin}
            - {name: roles, value: nvke-viewer}
apiAuthz:
  config:
    issuers:
      teleport:
        claims:
          - {name: roles, value: nvke-admin, grants: [admin]}
          - {name: roles, value: nvke-viewer, grants: [viewer]}
```

`issuer` is the Teleport cluster name, not a URL, and `<APP_URI>` is the application's URI in Teleport. Give each site's application its own URI: the URI is the token's audience, so two applications that share one accept each other's tokens. Teleport sends the token in its `Teleport-Jwt-Assertion` header, so configure the application to rewrite the `Authorization` header to `Bearer {{internal.jwt}}`.

## Turn on authorization in permissive mode

1. Add the `apiAuthz` values from [Grant roles](#grant-roles) to `auth-values.yaml`, with `enabled: true` and `permissive: true`. Merge them into the existing file rather than writing a second one.

2. Run the install command again, with the same flags as in [Secure the NVKE API](api-authentication.md#turn-on-authentication-in-permissive-mode), step 3.

   ```bash
   deploy/nvke-deploy.sh install --deploy-path deploy/nico --sitespec ../sitespec.yaml --stack-config ../stack-config.json --kubeconfig ../<CLUSTER_NAME>.kubeconfig \
     --values-for controller=../auth-values.yaml
   ```

   The chart refuses to render if `apiAuthz.enabled` is true while `apiAuth.enabled` is false, or if `apiAuthz.permissive` is false while `apiAuth.permissive` is true.

3. Wait for the rollout and check the startup log.

   ```bash
   kubectl -n nvke-controller rollout status deploy/nvke-controller
   ```

   ```bash
   kubectl -n nvke-controller logs deploy/nvke-controller | grep 'API authorization'
   ```

   The output contains an `API authorization is enabled` line with `"mode":"permissive"`. If the pod does not become ready, its log contains `Failed to set up API authorization` with every problem in the bundle.

## Find callers that enforcing mode would refuse

1. Read the authorization counter.

   ```bash
   kubectl -n nvke-controller exec deploy/nvke-controller -- wget -qO- http://127.0.0.1:9090/metrics | grep '^nvke_authz_decisions_total'
   ```

   Every series with an `outcome` other than `allow` counts requests that enforcing mode would refuse, by `action`.

2. Find the caller behind each one in the log.

   ```bash
   kubectl -n nvke-controller logs deploy/nvke-controller | grep 'authorization would have refused this request'
   ```

   Each line names the caller's `subject` and `issuer`, the `action`, and the request path as `resource_id`. NVKE never logs the token or its claims.

3. Grant each caller the role it needs, and run the install again as in step 2 of [Turn on authorization in permissive mode](#turn-on-authorization-in-permissive-mode).

Continue when only `outcome="allow"` series increase.

## Switch to enforcing mode

Authentication must enforce first: with authentication permissive, a request without a valid token reaches authorization with no caller, and enforcing authorization refuses it with `403` rather than `401`.

1. In `auth-values.yaml`, set `apiAuth.permissive: false` if it is not already, and `apiAuthz.permissive: false`. Run the install again with the same flags.

2. Wait for the rollout.

   ```bash
   kubectl -n nvke-controller rollout status deploy/nvke-controller
   ```

   In enforcing mode, NVKE logs each refused request as `refused request`, with the same fields as the permissive-mode message.

## Change grants

Change `apiAuthz.config`, `apiAuthz.policies` or `apiAuthz.roles` in `auth-values.yaml` and run the install again. NVKE needs no restart: the kubelet updates the mounted bundle within a minute or two, and NVKE reloads it every 10 seconds. A bundle that fails its checks does not replace the live one: NVKE keeps the last bundle that loaded, logs `Failed to reload the authorization bundle; keeping the live one`, and counts `nvke_authz_bundle_reload_failures_total`.

## Verify

1. Check the mode the controller reports.

   ```bash
   kubectl -n nvke-controller exec deploy/nvke-controller -- wget -qO- http://127.0.0.1:9090/metrics | grep '^nvke_authz_enabled'
   ```

   The output is `nvke_authz_enabled{mode="enforcing"} 1`.

2. With a token from a `viewer`, list the workload clusters, then try to delete a cluster that does not exist.

   ```bash
   curl -s -w '\n%{http_code}\n' -H "Authorization: Bearer ${TOKEN}" <API_URL>/api/clusters
   ```

   ```bash
   curl -s -w '\n%{http_code}\n' -X DELETE -H "Authorization: Bearer ${TOKEN}" <API_URL>/api/clusters/00000000-0000-0000-0000-000000000000
   ```

   The list returns `200`. The delete returns `403` with `{"message":"Not authorized"}`, because authorization runs before NVKE looks the cluster up. With an `admin` token, the same delete returns `404`.

## Metrics

| Metric | Labels | What it tells you |
|---|---|---|
| `nvke_authz_decisions_total` | `outcome`, `action` | Authorization decisions. `outcome` is `allow`, `deny`, `error` (NVKE could not decide, and refused), or `unauthenticated` (no authenticated caller). In permissive mode, a refusing outcome counts a request that was served. |
| `nvke_authz_enabled` | `mode` | `1` for the configured mode: `off`, `permissive` or `enforcing`. |
| `nvke_authz_bundle_reload_failures_total` | none | Failed bundle reloads. A rising count means a bundle change is not in effect. |
