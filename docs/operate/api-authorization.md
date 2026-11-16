# Authorize API callers by role

This page describes how to limit what each caller of the NVIDIA Kubernetes Engine (NVKE) API can do, by giving it a role. Do this after [Secure the NVKE API](api-authentication.md): authorization decides what an admitted caller may do, so it needs authentication on.

The NVKE controller checks every API operation before it serves it, by the operation's `operationId`, such as `listClusters` or `deleteCluster`. The API spec lists every `operationId`: it is `controller/api/openapi.yaml` in the checkout, and NVKE serves it without a token at `<API_URL>/openapi.yaml`. The paths that need no token, such as `/healthz` and `/api/version`, are not operations and are not checked.

The chart ships two roles. A role applies to every workload cluster: NVKE has no per-cluster permissions.

- **`admin`**: may call every operation.
- **`viewer`**: may call the `get` and `list` operations, such as `getCluster` and `listNodePools`.

A grant gives a role to the callers whose token matches it. A caller holds every role that any of its grants gives, and a caller with no role is refused every operation. You normally write only grants.

| Mode | Behavior |
|---|---|
| Off | Every admitted caller can call every operation. This is the default. |
| Permissive | Every request is checked, counted and logged, then served. Nothing is refused. |
| Enforcing | An admitted caller whose roles do not allow the operation gets `403` with `{"message":"Not authorized"}`. |

A token that authentication rejects still gets `401`, or `403` with `Not admitted: ...`, before authorization runs.

## Before you begin

- You have completed [Secure the NVKE API](api-authentication.md), and its Helm values are in `auth-values.yaml`, in the directory above the `nvke/` checkout. Authentication may still be permissive.
- You run the commands on this page from the root of the `nvke/` checkout, with `KUBECONFIG` set as in [Before you begin](api-authentication.md#before-you-begin) on that page.
- You know which group, or which service account, identifies each caller of the API.

## Grant roles

Grants live under `apiAuthz.authorizer.localBundle.config.issuers`, keyed by the `name` of an issuer in `apiAuth.config.issuers`. Each issuer takes two kinds of grant:

- **`claims`**: a token whose claim named `name` equals `value`, or is an array containing it, gets the roles in `grants`. This is the same match as `admit.claims` in authentication. Grants have no `groups` shorthand: to grant by group, use `claims` with `name: groups`. Use a claim the identity provider (IdP) sets, not one users can edit.
- **`serviceAccounts`**: a Kubernetes service account token from `namespace` gets the roles in `grants`. Set `name` to match one service account; omit `name` to match every service account in the namespace.

For the IdP from [Secure the NVKE API](api-authentication.md), grant `admin` and `viewer` by group:

```yaml
apiAuthz:
  enabled: true
  permissive: true
  authorizer:
    localBundle:
      config:
        issuers:
          example-idp:
            claims:
              - {name: groups, value: <ADMITTED_GROUP>, grants: [admin]}
              - {name: groups, value: <VIEWER_GROUP>, grants: [viewer]}
```

`<ADMITTED_GROUP>` is the group that authentication admits. `<VIEWER_GROUP>` is a group whose members get read-only access.

A grant applies only to callers that authentication admits, and authentication admits only `<ADMITTED_GROUP>`. Add `<VIEWER_GROUP>` to `admit.groups` of the existing `example-idp` issuer:

```yaml
apiAuth:
  config:
    issuers:
      - name: example-idp
        # Keep issuer, jwks and algorithms as they are.
        admit:
          groups:
            - <ADMITTED_GROUP>
            - <VIEWER_GROUP>
```

Edit the issuer in place. Helm replaces a list as a whole, so a second `issuers` list drops every field you leave out of it.

NVKE checks the grants at startup and on every reload. It refuses an issuer that `apiAuth` does not configure, a grant of a role the bundle does not declare, and a value for the tenancy keys `operatorTenant`, `tenant` and `operators`, which this API does not use.

### Grant a role to a service account

Once authorization enforces, every caller that authentication admits needs a role, including automation. One such caller is management-engine, a controller in the `management-engine-system` namespace that manages workload clusters through the NVKE API. The install on these pages does not deploy it. A service account's token has the subject `system:serviceaccount:<NAMESPACE>:<SERVICE_ACCOUNT>`. If that subject appears in the permissive-mode log in [Find callers that enforcing mode would refuse](#find-callers-that-enforcing-mode-would-refuse), the service account calls the API and needs a grant.

Grant it under the issuer that admits it. The issuer key, `local-kubernetes` here, must equal the `name` of an `apiAuth` issuer that admits Kubernetes service account tokens. [Secure the NVKE API](api-authentication.md) does not set one up; [Configuring NVKE API Authentication](../../controller/docs/configuring-api-authentication.md) describes it.

```yaml
apiAuthz:
  enabled: true
  permissive: true
  authorizer:
    localBundle:
      config:
        issuers:
          example-idp:
            claims:
              - {name: groups, value: <ADMITTED_GROUP>, grants: [admin]}
              - {name: groups, value: <VIEWER_GROUP>, grants: [viewer]}
          local-kubernetes:
            serviceAccounts:
              - {namespace: <NAMESPACE>, name: <SERVICE_ACCOUNT>, grants: [admin]}
```

`<NAMESPACE>` and `<SERVICE_ACCOUNT>` are the namespace and name of the caller's service account.

## Turn on authorization in permissive mode

1. Add the `apiAuthz` and `apiAuth` values from [Grant roles](#grant-roles) to `auth-values.yaml`. Merge them into the existing file rather than writing a second one.

2. Run the install command again, with the same flags as in [Secure the NVKE API](api-authentication.md#turn-on-authentication-in-permissive-mode), step 3.

   ```bash
   deploy/nvke-deploy.sh install --deploy-path deploy/nico --sitespec ../sitespec.yaml --stack-config ../stack-config.json --kubeconfig ../<CLUSTER_NAME>.kubeconfig \
     --values-for controller=../auth-values.yaml
   ```

   The chart refuses to render if `apiAuthz.enabled` is true while `apiAuth.enabled` is false, or if `apiAuthz.permissive` is true while `apiAuthz.enabled` is false.

3. Wait for the rollout.

   ```bash
   kubectl -n nvke-controller rollout status deploy/nvke-controller
   ```

4. Check the startup log.

   ```bash
   kubectl -n nvke-controller logs deploy/nvke-controller | grep 'API authorization'
   ```

   The output contains an `API authorization is enabled` line with `"mode":"permissive"`. If the pod does not become ready, its log contains `Failed to set up API authorization` with the problems it found in the bundle.

## Find callers that enforcing mode would refuse

1. Read the authorization counter.

   ```bash
   kubectl -n nvke-controller exec deploy/nvke-controller -- wget -qO- http://127.0.0.1:9090/metrics | grep '^nvke_authz_decisions_total'
   ```

   Every series with an `outcome` other than `allow` counts requests that enforcing mode would refuse, by `action`.

2. Find the caller behind each one in the log.

   ```bash
   kubectl -n nvke-controller logs deploy/nvke-controller | grep -E 'would have refused this request|failed to authorize request'
   ```

   Each line names the `outcome`, the `action` and the request path as `resource_id`. NVKE never logs the token or its claims. Act on each line by its `outcome`:

   | `outcome` | Meaning | Fix |
   |---|---|---|
   | `deny` | The caller, named by `subject` and `issuer`, holds no role that allows the action. | Grant it a role in the next step. |
   | `unauthenticated` | The request has no authenticated caller, so the line names none. | Fix the caller's authentication, as in [Secure the NVKE API](api-authentication.md#find-callers-that-enforcing-mode-would-reject). |
   | `error` | NVKE could not decide. The line is `failed to authorize request`. | Read its `error` field. |

3. In `auth-values.yaml`, grant each `deny` caller the role it needs. For a user, look the `subject` up in the IdP and grant a group the user belongs to. For a service account, the `subject` is `system:serviceaccount:<NAMESPACE>:<SERVICE_ACCOUNT>`; grant it under `serviceAccounts`, as in [Grant a role to a service account](#grant-a-role-to-a-service-account).

4. Run the install again with the same flags.

   ```bash
   deploy/nvke-deploy.sh install --deploy-path deploy/nico --sitespec ../sitespec.yaml --stack-config ../stack-config.json --kubeconfig ../<CLUSTER_NAME>.kubeconfig \
     --values-for controller=../auth-values.yaml
   ```

   NVKE picks up grants without a restart, as described in [Change grants](#change-grants).

Continue when only `outcome="allow"` series increase.

## Switch to enforcing mode

Authentication must enforce before authorization does. With authentication permissive, a request without a valid token reaches authorization with no caller, and enforcing authorization refuses it with `403` rather than `401`, so the chart refuses to render `apiAuthz.permissive: false` while `apiAuth.permissive` is true. You can switch both in one install.

1. In `auth-values.yaml`, set `apiAuthz.permissive: false`, and `apiAuth.permissive: false` if it is not already. Run the install again with the same flags.

   ```bash
   deploy/nvke-deploy.sh install --deploy-path deploy/nico --sitespec ../sitespec.yaml --stack-config ../stack-config.json --kubeconfig ../<CLUSTER_NAME>.kubeconfig \
     --values-for controller=../auth-values.yaml
   ```

2. Make sure NVKE restarted. NVKE reads the mode from the `nvke-controller-config` ConfigMap only at startup. The chart rolls the pod when that ConfigMap changes; without the chart, restart it:

   ```bash
   kubectl -n nvke-controller rollout restart deploy/nvke-controller
   ```

3. Wait for the rollout.

   ```bash
   kubectl -n nvke-controller rollout status deploy/nvke-controller
   ```

   In enforcing mode, NVKE logs each refused request as `refused request`, with the same fields as the permissive-mode message, or as `failed to authorize request` when NVKE could not decide.

If callers get an unexpected `403` with `{"message":"Not authorized"}`, set `apiAuthz.permissive: true`, run the install again and restart NVKE, as in steps 1 to 3.

## Verify

1. Check the mode NVKE reports.

   ```bash
   kubectl -n nvke-controller exec deploy/nvke-controller -- wget -qO- http://127.0.0.1:9090/metrics | grep '^nvke_authz_enabled'
   ```

   The output is `nvke_authz_enabled{mode="enforcing"} 1`. If the mode is `permissive`, NVKE has not restarted since the change: restart it as in step 2 of [Switch to enforcing mode](#switch-to-enforcing-mode).

2. Run `kubectl oidc-login clean` to drop the cached token. Sign in to the IdP as a member of `<VIEWER_GROUP>` who is not in `<ADMITTED_GROUP>`, and get a token into `TOKEN` as in [Secure the NVKE API](api-authentication.md#with-curl), step 1 of "With curl". `<API_URL>` is the address described in step 2 of the same section.

3. List the workload clusters.

   ```bash
   curl -s -w '\n%{http_code}\n' -H "Authorization: Bearer ${TOKEN}" <API_URL>/api/clusters
   ```

   The response is `200`.

4. Try to delete a cluster that does not exist.

   ```bash
   curl -s -w '\n%{http_code}\n' -X DELETE -H "Authorization: Bearer ${TOKEN}" <API_URL>/api/clusters/00000000-0000-0000-0000-000000000000
   ```

   The response is `403` with `{"message":"Not authorized"}`, because authorization runs before NVKE looks the cluster up. With a token from a member of `<ADMITTED_GROUP>`, the same delete returns `404`.

## Change grants

NVKE applies grant changes without a restart: the kubelet updates the mounted bundle within a minute or two, and NVKE reloads it every 10 seconds.

1. Change the grants under `apiAuthz.authorizer.localBundle.config` in `auth-values.yaml`.

2. Run the install again with the same flags.

   ```bash
   deploy/nvke-deploy.sh install --deploy-path deploy/nico --sitespec ../sitespec.yaml --stack-config ../stack-config.json --kubeconfig ../<CLUSTER_NAME>.kubeconfig \
     --values-for controller=../auth-values.yaml
   ```

3. Wait two minutes for the kubelet to update the bundle, then check that NVKE loaded the change.

   ```bash
   kubectl -n nvke-controller logs deploy/nvke-controller --since=3m | grep 'authorization bundle' | tail -n 1
   ```

   The output is `Reloaded the authorization bundle`, and requests from the callers you changed get the new decisions. A bundle that fails its checks does not replace the live one: NVKE keeps the last bundle that loaded, logs `Failed to reload the authorization bundle; keeping the live one`, and counts `nvke_authz_bundle_reload_failures_total`.

A change to `apiAuth` restarts NVKE instead: the chart rolls the pod, and NVKE loads the new bundle at startup.

## Customize roles

To change what a role allows, or to add a role, set `apiAuthz.authorizer.localBundle.policies` (Cedar policies) and `apiAuthz.authorizer.localBundle.roles` (the role list) in `auth-values.yaml`. The defaults are in the chart's `values.yaml`. A policy names actions as `NVKE::Action::"<operationId>"`, and NVKE refuses a policy that names an action the API does not have. NVKE reloads policies and roles the same way as grants, as in [Change grants](#change-grants).

## Use Teleport application access

This section applies only if callers reach the API through Teleport application access. Teleport signs a JSON Web Token (JWT) for each request to an application, with the user's Teleport roles in its `roles` claim, and has no `groups` claim. Admit those tokens by role in `apiAuth`, and grant NVKE roles by the same claim:

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
  authorizer:
    localBundle:
      config:
        issuers:
          teleport:
            claims:
              - {name: roles, value: nvke-admin, grants: [admin]}
              - {name: roles, value: nvke-viewer, grants: [viewer]}
```

- `<TELEPORT_CLUSTER_NAME>` is the Teleport cluster name, not a URL. Teleport signs it into the token as the issuer.
- `<APP_URI>` is the application's URI in Teleport, and the token's audience. Give the application for each management cluster its own URI: two applications that share one accept each other's tokens.
- `<TELEPORT_PROXY_ADDRESS>` is the address of the Teleport proxy.

Add the `teleport` issuer to the existing `apiAuth.config.issuers` list, and its grants beside the existing ones, rather than replacing them.

In the Teleport application, leave `rewrite.jwt_claims` at its default, `roles-and-traits`, or set it to `roles`, so the token carries `roles`. Teleport sends the token in its `Teleport-Jwt-Assertion` header, so also rewrite the `Authorization` header. In `teleport.yaml`:

```yaml
rewrite:
  headers:
    - "Authorization: Bearer {{internal.jwt}}"
```

In a dynamic `app` resource:

```yaml
spec:
  rewrite:
    headers:
      - name: Authorization
        value: "Bearer {{internal.jwt}}"
```

## Next steps

- [Create a workload cluster](../workload-clusters/create.md).
- [Observe NVKE and its clusters](observe.md).
