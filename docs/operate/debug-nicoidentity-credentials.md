# Debug NicoIdentity credential health

This page describes how to read the credential check that CAPNICo, the Cluster API (CAPI) infrastructure provider for NVIDIA Infra Controller (NICo), publishes on a `NicoIdentity` object, and what to do when the result is not healthy. Use it when you read `NicoIdentity` objects with `kubectl` instead of `nvkectl get providers`, or when the `CREDENTIALS` column of `nvkectl get providers` does not say enough.

CAPNICo checks the credentials Secret that each `NicoIdentity` names: it authenticates with the credentials and reads the current tenant of the organization in the Secret from the NICo API. It writes the result to the `Ready` condition and `status.lastCheckedTime` of the `NicoIdentity`. The check itself never changes which credentials provisioning uses; `spec.credentialsRef` does. `nvkectl get providers` reads the same status; [Infrastructure providers](https://github.com/dsx-ai-factory/nvke/blob/main/cli/README.md#infrastructure-providers) lists its `CREDENTIALS` values.

## Before you begin

- You have completed [Configure an infrastructure provider: NICo](provider-nico.md).
- To have a result to read, the CAPNICo release on the management cluster must serve `NicoIdentity`. The release the NVIDIA Kubernetes Engine (NVKE) install deploys, `v0.0.45` in [`deploy/nico/capnico/capnico.yaml`](https://github.com/dsx-ai-factory/nvke/blob/main/deploy/nico/capnico/capnico.yaml), does not, and `nvkectl get providers` reports `CREDENTIALS` as `UNSUPPORTED` with it.
- `kubectl` uses the management cluster's admin kubeconfig, for example with `export KUBECONFIG=<CLUSTER_NAME>.kubeconfig`. A narrower kubeconfig needs `get` and `list` on `nicoidentities.infrastructure.cluster.x-k8s.io` to read results, and read access to the `controller-manager` Deployment, its pods and their logs in `capnico-system` to read the manager log.
- You have `jq`, for the log commands.

## Find the NicoIdentity

1. List every `NicoIdentity`.

   ```bash
   kubectl get nicoidentities -A
   ```

   The output has the columns `READY`, `LASTCHECKED` and `AGE`:

   ```text
   NAMESPACE        NAME           READY   LASTCHECKED   AGE
   capnico-system   nico-default   False   2m            3d
   ```

   `READY` is the status of the `Ready` condition, `LASTCHECKED` is how long ago the last check finished, and `AGE` is how long ago the `NicoIdentity` was created. If the command returns no rows or an error, go to [No result at all](#no-result-at-all).

2. Find the `NicoIdentity` that names the Secret you want to check. By default, CAPNICo uses the `nico-credentials` Secret in `capnico-system` for every `NicoCluster` that names no credentials of its own.

   ```bash
   kubectl get nicoidentities -A -o jsonpath='{range .items[*]}{.metadata.namespace}{"\t"}{.metadata.name}{"\t"}{.spec.credentialsRef.name}{"\n"}{end}'
   ```

   Each line shows the namespace, the name and the Secret the `NicoIdentity` names. The Secret is always in the namespace of the `NicoIdentity`.

## Check that the result is current

A result describes the `NicoIdentity` as it is now only when two things hold:

- The `Ready` condition's `observedGeneration` equals the object's `metadata.generation`. A lower `observedGeneration` is the result for an earlier spec, for example before `spec.credentialsRef` changed.
- `status.lastCheckedTime` is recent. CAPNICo checks each `NicoIdentity` again four to five minutes after the last check finished, including a failed one, so a time older than two check intervals, 10 minutes, usually means nothing is checking it. `nvkectl get providers` reports `STALE` from that age.

The condition's `lastTransitionTime` is not a freshness signal: it changes only when `status` changes, and a `NicoIdentity` that has failed the same way for a day keeps the time of its first failure.

1. Print the generation, the observed generation and the check time.

   ```bash
   kubectl get nicoidentity <NAME> -n <NAMESPACE> -o jsonpath='{.metadata.generation}{"\t"}{.status.conditions[?(@.type=="Ready")].observedGeneration}{"\t"}{.status.lastCheckedTime}{"\n"}'
   ```

   Replace `<NAME>` and `<NAMESPACE>` with the values from [Find the NicoIdentity](#find-the-nicoidentity). The first two numbers match, and the time is less than 10 minutes before the current time:

   ```bash
   date -u +%Y-%m-%dT%H:%M:%SZ
   ```

   For a few moments after a check, `lastCheckedTime` can still show the previous check, because CAPNICo writes the condition and the time in separate requests.

2. If the numbers differ, wait a few seconds and run the command again. A spec change starts a check within seconds. If they still differ after 10 minutes, or the time is older than 10 minutes, go to [The result is stale](#the-result-is-stale). If the object has no `status`, go to [No result at all](#no-result-at-all).

`kubectl wait --for=condition=Ready` waits for the condition status only and does not check its age, so it can succeed on a result that is hours old.

## Read the reason and fix it

1. Print the `Ready` condition.

   ```bash
   kubectl get nicoidentity <NAME> -n <NAMESPACE> -o yaml
   ```

   The `status` section holds the result:

   ```yaml
   status:
     conditions:
     - lastTransitionTime: "2026-01-15T10:02:11Z"
       message: Current-tenant lookup rejected the access token.
       observedGeneration: 1
       reason: AuthenticationFailed
       status: "False"
       type: Ready
     lastCheckedTime: "2026-01-15T10:22:40Z"
   ```

2. Find the `reason` in the table and apply the fix. `False` means the check found a configuration, credential or permission failure. `Unknown` means the check could not decide.

   | Status | Reason | Message | Fix |
   |---|---|---|---|
   | `True` | `ValidationSucceeded` | `Authentication and current-tenant lookup succeeded.` | Nothing. The check does not test provisioning permissions or capacity. If the message adds `TLS certificate verification is disabled.`, the Secret sets `insecureSkipTLSVerify` and CAPNICo sends the credentials without verifying the server certificate: add the NICo CA bundle to the Secret as `ca.crt` and remove `insecureSkipTLSVerify`. |
   | `False` | `CredentialsNotFound` | `Credential Secret was not found.` | Create the Secret that `spec.credentialsRef.name` names, in the namespace of the `NicoIdentity`, or correct the name. For `nico-credentials`, see [Manage the NICo credentials](provider-nico.md#manage-the-nico-credentials). Prefer creating the missing Secret: changing `spec.credentialsRef` also changes the credentials of every `NicoCluster` whose `identityRef` names this `NicoIdentity`, and a Secret for another NICo organization can orphan their instances. If you rename, name a Secret for the same NICo organization. |
   | `False` | `InvalidConfiguration` | One of: `Credential configuration is missing required fields or contains incompatible or invalid values.`, `Credential endpoint configuration requires absolute HTTP or HTTPS URLs.`, `Credential TLS configuration does not contain a usable CA bundle.`, `Credential client configuration is invalid.` | Correct the Secret. It needs `endpoint` and `orgID`, and either `token` or all of `tokenURL`, `clientID` and `clientSecret`, not both. `endpoint` and `tokenURL` are absolute `http` or `https` URLs, and `ca.crt`, if set, is a PEM CA bundle. `insecureSkipTLSVerify`, if set, is `true` or `false`. |
   | `False` | `AuthenticationFailed` | `Current-tenant lookup rejected the access token.` or `Token issuer rejected the client credentials.` | NICo, or the OAuth token issuer, rejected the credentials. Replace the token, or the client ID and secret, as described in [Manage the NICo credentials](provider-nico.md#manage-the-nico-credentials). |
   | `False` | `AccessDenied` | `Current-tenant lookup was forbidden.` | NICo accepted the credentials and refused the request for the organization in `orgID`. Check `orgID`, and the access of the principal the credentials belong to in NICo. |
   | `Unknown` | `SecretReadFailed` | `Credential Secret could not be read.` | The CAPNICo manager's direct read of the Secret failed, for example with an API server error or a timeout. Read the `error` key in the [manager log](#read-the-capnico-manager-log). |
   | `Unknown` | `ValidationFailed` | `<step> exceeded the validation deadline.`, `<step> failed TLS certificate verification.` or `<step> could not establish credential health.` | `<step>` is `Token acquisition`, `Current-tenant lookup` or `Credential validation`. Check `endpoint` and `tokenURL`, the network path from the management cluster to NICo, and `ca.crt` for a TLS failure. A check stops after `--provider-identity-validation-timeout`, 30 seconds by default. `could not establish credential health` also covers an unexpected response from NICo or the token issuer. |

   The `NicoIdentity` check writes no NICo client error to its status or the log, because such an error can contain credentials. The message is the most detail a check reports.

3. After you change the Secret, check the result again as in [Check that the result is current](#check-that-the-result-is-current). Creating, changing or deleting the Secret starts a check of the `NicoIdentity` objects that name it within seconds.

## The result is stale

A result is stale when no check has finished for more than 10 minutes. The `NicoIdentity` keeps its last result, which then ages; nothing marks it out of date. Check these causes in order:

1. The manager is running.

   ```bash
   kubectl -n capnico-system get deploy controller-manager
   ```

   The Deployment shows all replicas ready. If it does not, read the pod's state with `kubectl -n capnico-system get pods`.

   A CAPNICo manager that serves `NicoIdentity` exits at startup when the `NicoIdentity` custom resource definition (CRD) is missing. Its log then shows `unable to create controller` with `"controller":"NicoIdentity"`, and the pod restarts in `CrashLoopBackOff`; while it does, CAPNICo reconciles nothing. Install the CRD from the same CAPNICo release. On an NVKE management cluster, the CRDs come from `capnico.yaml` in the deploy path; with `--release`, the manager image comes from the release instead, so check that the two versions match. See step 3. The next restart succeeds, or restart the Deployment to skip the restart delay:

   ```bash
   kubectl -n capnico-system rollout restart deploy/controller-manager
   ```

   A manager without `list` and `watch` on Secrets in its scope also fails to start its controllers and exits, so missing Secret access shows up as a restarting pod, not as a `SecretReadFailed` result.

   A manager that starts checks every `NicoIdentity` in its scope.

2. The manager's scope includes the `NicoIdentity`. Print the manager's arguments:

   ```bash
   kubectl -n capnico-system get deploy controller-manager -o jsonpath='{.spec.template.spec.containers[?(@.name=="manager")].args}'
   ```

   - With `--namespace=<WATCH_NAMESPACE>`, the manager checks only `NicoIdentity` objects in `<WATCH_NAMESPACE>`. Create the `NicoIdentity` in that namespace, next to its Secret.
   - With `--watch-filter=<VALUE>`, the manager checks only `NicoIdentity` objects with the label `cluster.x-k8s.io/watch-filter=<VALUE>`. A `NicoIdentity` that loses the label keeps its last result. Add the label:

     ```bash
     kubectl label nicoidentity <NAME> -n <NAMESPACE> cluster.x-k8s.io/watch-filter=<VALUE> --overwrite
     ```

     The label change starts a check within seconds.

3. The manager's image serves `NicoIdentity`. A manager from an earlier CAPNICo release, such as `v0.0.45`, has no `NicoIdentity` controller and never checks one, even when the CRD is installed. Upgrade CAPNICo as a whole, CRDs and manager from the same release. On an NVKE management cluster, CAPNICo comes from [`deploy/nico/capnico/capnico.yaml`](https://github.com/dsx-ai-factory/nvke/blob/main/deploy/nico/capnico/capnico.yaml). Install from an NVKE checkout or release whose `capnico.yaml` pins a CAPNICo release that serves `NicoIdentity`. Bumping it changes `capnico.yaml`, the image tag in [`deploy/nico/capnico/release.yaml`](https://github.com/dsx-ai-factory/nvke/blob/main/deploy/nico/capnico/release.yaml) and `controller/go.mod` together, and keeps the manager's `--reboot-annotation` and `--repair-annotation` arguments. The upstream release starts the manager without those arguments, and it then ignores every NVKE reboot and repair request. When you install with `--release`, the manager image comes from the release's `capnico-controller` artifact. Re-running the NVKE install applies whatever CAPNICo that build or release carries.

During a NICo outage a round of checks takes longer: CAPNICo checks one `NicoIdentity` at a time, and each check can run for the full validation timeout. With many `NicoIdentity` objects, a gap longer than 10 minutes can then mean slow checks rather than stopped ones; the `checked NicoIdentity credentials` lines in the [manager log](#read-the-capnico-manager-log) tell the two apart.

## No result at all

`kubectl get nicoidentities -A` returns no rows, an error, or a `NicoIdentity` with no `status`:

- **No `NicoIdentity` names the Secret.** Nothing checks a Secret that no `NicoIdentity` names, and installing CAPNICo creates none. Create one in the namespace of the Secret, for example for `nico-credentials`:

  ```yaml
  apiVersion: infrastructure.cluster.x-k8s.io/v1alpha1
  kind: NicoIdentity
  metadata:
    name: nico-default
    namespace: capnico-system
  spec:
    credentialsRef:
      name: nico-credentials
  ```

  Save it as `nico-identity.yaml` and apply it with `kubectl apply -f nico-identity.yaml`. Creating the `NicoIdentity` starts its first check. If the manager runs with `--watch-filter`, add the label described in [The result is stale](#the-result-is-stale).

- **The installed CAPNICo does not serve `NicoIdentity`.** `kubectl` prints `the server doesn't have a resource type "nicoidentities"`. Confirm it:

  ```bash
  kubectl get crd nicoidentities.infrastructure.cluster.x-k8s.io
  ```

  `NotFound` means the CRD is not installed. The CAPNICo release the NVKE install deploys does not include it.

- **Your kubeconfig may not list `NicoIdentity` objects.** `kubectl` returns `Forbidden`. Confirm it:

  ```bash
  kubectl auth can-i list nicoidentities.infrastructure.cluster.x-k8s.io -n capnico-system
  ```

  CAPNICo releases that serve `NicoIdentity` ship the ClusterRole `nicoidentity-viewer-role`, in the release bundle and in the chart while `rbac.helpers.enabled` is `true`, its default. It grants reads of `NicoIdentity` objects and their status, and no access to Secrets. Bind it to the user that reads the results:

  ```bash
  kubectl create rolebinding nicoidentity-viewer -n capnico-system --clusterrole=nicoidentity-viewer-role --user=<USER>
  ```

  Replace `<USER>` with the user name your kubeconfig authenticates as. The RoleBinding grants reads in `capnico-system` only, so list with `-n capnico-system` instead of `-A`, or bind the ClusterRole with a ClusterRoleBinding to read every namespace. With the chart's `rbac.namespaced=true`, the role is a Role in the release namespace; use `--role=nicoidentity-viewer-role` instead of `--clusterrole`.

- **The first check has not finished.** A `NicoIdentity` has no `status` until its first check completes, and no `status` is not a successful check. The first check starts when the `NicoIdentity` is created. If there is still no `status` after 10 minutes, see [The result is stale](#the-result-is-stale).

## Read the CAPNICo manager log

The `NicoIdentity` status and the manager log are the only places a check reports to. A failed check changes the status and raises no Kubernetes Event. No CAPNICo metric reports check results: `capnico_api_requests_total` and `capnico_api_request_duration_seconds` count the request a check sends to NICo under `operation="get_current_tenant"`, together with the same request from cluster and machine reconciliation, and a check that fails before that request, on the Secret or at token acquisition, adds nothing to them.

1. Show the checks the manager finished in the last hour.

   ```bash
   kubectl logs -n capnico-system deploy/controller-manager --since=1h \
     | jq -cR 'fromjson? | select(.msg == "checked NicoIdentity credentials")'
   ```

   Each line names the `NicoIdentity` in `namespace` and `name`, and carries the result in `ready` and `reason`. If the log has no line for a `NicoIdentity` in the last 10 minutes, the manager is usually not checking it; see [The result is stale](#the-result-is-stale), which also covers slow checks during a NICo outage.

2. Show the Secret read errors.

   ```bash
   kubectl logs -n capnico-system deploy/controller-manager --since=1h \
     | jq -cR 'fromjson? | select(.msg == "failed to read NicoIdentity credential Secret")'
   ```

   The `error` key holds the error the Kubernetes API returned for a `SecretReadFailed` result, for example an API server error or a timeout.

## Verify

Run `kubectl get nicoidentities -A`. The `NicoIdentity` that names your credentials Secret shows `READY` as `True` and `LASTCHECKED` under 10 minutes, and the jsonpath command in [Check that the result is current](#check-that-the-result-is-current) prints the same number twice. If the Secret is the provider's credentials Secret, `nico-credentials` in `capnico-system` by default, the `CREDENTIALS` column of `nvkectl get providers` reads `HEALTHY`; `nvkectl` reports on no other Secret.

## Next steps

- [Manage the NICo credentials](provider-nico.md#manage-the-nico-credentials) to rotate the token.
- [Troubleshoot](troubleshoot.md) a provider state other than `CONFIGURED`.
- The [NicoIdentity API reference](https://github.com/dsx-ai-factory/cluster-api-provider-nico/blob/main/docs/api-reference.md#nicoidentity) in the CAPNICo repository for every field and reason.
