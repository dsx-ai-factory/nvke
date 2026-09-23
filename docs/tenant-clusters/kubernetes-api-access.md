# Tenant Kubernetes API Access

## Overview

NKE exposes each tenant cluster's Kubernetes API on the endpoint recorded in
that cluster's kubeconfig. Clients connect to that endpoint directly; neither
the NKE Controller nor `nkectl` proxies Kubernetes API traffic.

The preferred endpoint is a private kube-vip address shared by the control-plane
nodes. If the cluster has no control-plane VIP, NKE uses the first
control-plane node's IP address instead. That fallback is not load balanced and
loses API availability when that node or its network path is unavailable.

NKE does not create a public Kubernetes API endpoint. A deployment must provide
the routes and filtering that make the private endpoint reachable from approved
networks, such as the site VPC, tenant peering, or a VPN. The owner of peering,
VPN, firewall, and DNS configuration is deployment-specific and must be agreed
before tenant access is enabled.

## Principal Classes

The following table describes the alpha access model. "Tenant API endpoint"
means the control-plane VIP when configured and the first control-plane node IP
otherwise.

| Principal | Authentication | Authorization | Network path | Credential lifetime |
|---|---|---|---|---|
| Tenant operator | The operator obtains the kubeadm `admin.conf` with `nkectl get kubeconfig`. It contains an X.509 client certificate and private key. | The certificate identifies `kubernetes-admin` in the `kubeadm:cluster-admins` group, which has `cluster-admin`. | The operator connects directly to the tenant API endpoint from an approved private network. | One year. See [Alpha Operator Access](#alpha-operator-access) for renewal and retrieval details. |
| Tenant user | NKE alpha does not deliver a distinct, individually attributable tenant-user credential. A tenant operator must establish the user's authentication outside NKE. | A tenant operator creates the required Kubernetes RBAC bindings. NKE grants no default tenant-user role. | The user connects directly to the tenant API endpoint from an approved private network. | Determined by the tenant's chosen authentication mechanism. |
| Tenant automation | A tenant operator creates a Kubernetes ServiceAccount and uses a TokenRequest token, or configures another Kubernetes-supported authenticator. NKE does not distribute a separate automation credential. | A tenant operator binds the ServiceAccount or other identity to a workload-specific Role or ClusterRole. NKE grants no default role. | The automation connects directly to the tenant API endpoint, normally from the tenant network or from inside the cluster. | TokenRequest tokens are time limited. Their requested and maximum lifetimes are deployment policy rather than an NKE tenant-access setting. |
| NKE Controller | The NKE Controller's SPIFFE workload identity obtains a short-lived JWT-SVID, optionally exchanges it through the configured token-exchange service, and presents the resulting bearer token to the tenant API server. | The token's `sub` claim maps to a Kubernetes user. NKE binds that user to the `nke:cwe` ClusterRole and namespace-scoped Roles needed for lifecycle operations. | The NKE Controller connects from the Site Controller cluster directly to the tenant API endpoint over the site's private network. | JWT expiry is set by the issuing identity system. The client obtains a new token rather than storing a long-lived bearer token. |
| Cluster API controllers | Each controller uses its SPIFFE workload identity and the same JWT authentication path as the NKE Controller. | The current NKE manifest binds the CAPI manager identities to `cluster-admin`. | The controllers connect from the Site Controller cluster directly to the tenant API endpoint. | JWT expiry is set by the issuing identity system, and clients obtain replacement tokens as needed. |
| Break-fix system | NKE alpha defines no generic break-fix credential. A break-fix deployment must use a dedicated ServiceAccount or workload identity rather than the operator admin kubeconfig. | The deployer creates RBAC matching the repair operations the system performs. NKE grants no default break-fix role. | The system connects directly to the private tenant API endpoint from its approved runtime network. | Determined by the selected ServiceAccount or workload-identity mechanism. |
| Break-glass operator | The operator obtains the same kubeadm `admin.conf` used by the alpha tenant-operator path. Access to the management-cluster Secret is the delivery control. | `cluster-admin`. | The operator must have both management-cluster access to retrieve the kubeconfig and private network reachability to the tenant API endpoint. | The same one-year certificate and renewal behavior apply. The exported copy does not refresh or gain the renewed certificate automatically. Treat every downloaded copy as sensitive until its certificate expires or the cluster CA and credentials are replaced. |

The overlap between routine tenant-operator access and break-glass access is an
alpha limitation: both use the same highly privileged, non-attributable
credential. Do not share this kubeconfig with applications or commit it to a
repository.

## Alpha Operator Access

An operator first configures `nkectl` with a kubeconfig for the Site Controller
cluster. That management-cluster identity must be allowed to read the tenant
access Secret in `nke-system`.

```bash
nkectl config set-site --name "$SITE_NAME" \
  --url "$NKE_CONTROLLER_URL" \
  --kubeconfig site-controller.kubeconfig

nkectl get kubeconfig "$CLUSTER_NAME" --output-file tenant.kubeconfig
kubectl --kubeconfig tenant.kubeconfig get nodes
```

`nkectl get kubeconfig` reads
`nke-system/<cluster>-admin-kubeconfig` from the Site Controller cluster. The
Secret contains the verbatim `admin.conf` fetched from a tenant control-plane
node. The resulting `kubectl` process then connects directly to the server URL
inside that kubeconfig.

Retrieving the kubeconfig proves access to the management-cluster Secret; it
does not prove that the operator's workstation can route to the tenant API
endpoint.

The kubeadm client certificate embedded in `admin.conf` is valid for one year.
With the NKE defaults, the NKE Controller checks certificate expiration every
12 hours and renews control-plane certificates when their remaining lifetime is
less than 30 days. Renewal creates a new certificate valid for another year.
It updates the control-plane copy, not kubeconfig files that operators have
already downloaded. Operators must retrieve a refreshed kubeconfig before the
certificate in their local copy expires.

## Service Identity and Tenant OIDC

NKE's existing SPIFFE/OIDC integration is for service-to-cluster API access. It
is separate from the alpha human-operator flow.

For an OIDC-enabled tenant cluster, the tenant API server uses Kubernetes
structured authentication configuration. It validates the configured issuer,
signature, audience, and expiry and maps the JWT `sub` claim to the Kubernetes
username. Kubernetes RBAC then authorizes that username. The OIDC discovery
provider publishes discovery metadata and signing keys; it does not grant
permissions.

The NKE Controller publishes a separate `nke-system/<cluster>-kubeconfig` for
its own use. On an OIDC-enabled cluster, this kubeconfig runs the SPIFFE
exec-auth plugin to obtain a JWT. It is not the kubeconfig returned by
`nkectl get kubeconfig`.

## Authorization Model

Authentication establishes a Kubernetes username and, where applicable,
groups. Kubernetes RBAC makes the separate authorization decision:

- The alpha admin certificate maps to `kubernetes-admin` and the
  `kubeadm:cluster-admins` group, which is bound to `cluster-admin`.
- The NKE Controller's JWT `sub` maps to the user named in NKE's `nke:cwe`
  bindings. The identifier is retained in the current RBAC manifest. Its
  ClusterRole and namespace Roles enumerate the lifecycle operations the NKE
  Controller may perform.
- The CAPI manager JWT subjects are currently bound to `cluster-admin`.
- Tenant users, tenant automation, and break-fix systems receive no additional
  default NKE role. A tenant administrator must create explicit bindings for
  them.

RBAC authorization does not make an endpoint reachable, and network
reachability does not grant Kubernetes permissions. Both checks must succeed.

## Planned Human OIDC Access

After alpha, tenant operators and users are expected to authenticate through an
approved identity provider instead of sharing `admin.conf`. The intended flow
is:

1. The user authenticates to the identity provider.
2. The client obtains an audience-scoped, time-limited OIDC JWT.
3. The tenant Kubernetes API server validates the issuer, signature, audience,
   expiry, and claims.
4. The API server maps the JWT `sub` claim, and eventually approved group
   claims, to a Kubernetes username and groups.
5. Kubernetes RBAC grants only the roles bound to that identity.

The identity-provider integration, kubeconfig or exec-plugin delivery, claim
conventions, default tenant roles, group mapping, and revocation behavior are
future work. Until those contracts are implemented, they must not be presented
as available tenant access.

## Network Paths

### Site Controller to tenant cluster

The NKE Controller and the Cluster API controllers connect from the Site
Controller cluster to the private tenant API endpoint. This path is required
for cluster lifecycle and reconciliation. The Site Controller deployment must
be able to route to TCP port 6443 on the endpoint.

### In-VPC access

A client already on a network with a route to the tenant endpoint can connect
directly to TCP port 6443. TLS authenticates the API server, and the credentials
in the request authenticate the client.

### Tenant peering or VPN

An operator outside the directly connected network needs an approved private
path, such as tenant peering or a VPN. NKE records and configures the Kubernetes
API endpoint but does not, by itself, establish the operator's peering, VPN,
firewall, or DNS configuration.

### Public networks

NKE does not provision a public load balancer or public ingress for a tenant
Kubernetes API. The deployment network must reject access from untrusted and
public networks. This is a deployment property and must be verified in the
target environment rather than inferred from the kubeconfig address alone.

## Manual Validation Checklist

Run these checks against a non-production cluster from approved test locations.
Do not paste kubeconfig contents, client keys, or bearer tokens into tickets or
logs.

1. Create a temporary directory with automatic cleanup, then retrieve the alpha
   operator kubeconfig. Run the remaining commands in the same shell so the
   cleanup trap remains active:

   ```bash
   VALIDATION_DIR="$(mktemp -d)" || exit 1
   readonly VALIDATION_DIR
   TENANT_KUBECONFIG="${VALIDATION_DIR}/tenant.kubeconfig"
   TENANT_CA="${VALIDATION_DIR}/tenant-ca.pem"
   VALIDATION_NAMESPACE=
   cleanup_validation() {
     if [ -n "${VALIDATION_NAMESPACE}" ]; then
       kubectl --kubeconfig "$TENANT_KUBECONFIG" delete namespace \
         "$VALIDATION_NAMESPACE" --ignore-not-found --wait=false \
         >/dev/null 2>&1 || true
     fi
     rm -rf -- "${VALIDATION_DIR:?}"
   }
   trap cleanup_validation EXIT

   nkectl get kubeconfig "$CLUSTER_NAME" --output-file "$TENANT_KUBECONFIG"
   chmod 0600 "$TENANT_KUBECONFIG"
   ```

2. Confirm that the kubeconfig names the expected VIP or fallback node IP:

   ```bash
   TENANT_API_SERVER="$(kubectl --kubeconfig "$TENANT_KUBECONFIG" \
     config view --minify -o jsonpath='{.clusters[0].cluster.server}')"
   printf '%s\n' "$TENANT_API_SERVER"
   ```

   Inspect the embedded client certificate's exact validity window:

   ```bash
   kubectl --kubeconfig "$TENANT_KUBECONFIG" \
     config view --raw --flatten --minify \
     -o jsonpath='{.users[0].user.client-certificate-data}' \
     | openssl base64 -d -A \
     | openssl x509 -noout -subject -dates
   ```

3. From an approved network, verify authenticated API access and identity:

   ```bash
   kubectl --kubeconfig "$TENANT_KUBECONFIG" get --raw=/readyz
   kubectl --kubeconfig "$TENANT_KUBECONFIG" auth whoami
   kubectl --kubeconfig "$TENANT_KUBECONFIG" auth can-i '*' '*'
   ```

   The alpha admin kubeconfig must reach the API, identify the kubeadm admin
   user, and report that all actions are allowed.

4. Verify that an unauthenticated request cannot list tenant resources. Use the
   tenant CA but do not supply a client certificate or bearer token:

   ```bash
   kubectl --kubeconfig "$TENANT_KUBECONFIG" \
     config view --raw --flatten --minify \
     -o jsonpath='{.clusters[0].cluster.certificate-authority-data}' \
     | openssl base64 -d -A > "$TENANT_CA"

   curl --silent --show-error --output /dev/null --write-out '%{http_code}\n' \
     --cacert "$TENANT_CA" "$TENANT_API_SERVER/api/v1/nodes"
   ```

   The request must not return a successful node list. Depending on the
   API-server anonymous-authentication configuration, rejection can be HTTP 401
   or HTTP 403. The cleanup trap removes both temporary artifacts when
   the shell exits.

5. From an authorized Site Controller workload environment with access to its
   SPIRE socket and tenant service kubeconfig, verify the service identity and
   effective RBAC:

   ```bash
   kubectl --kubeconfig tenant-service.kubeconfig auth whoami
   kubectl --kubeconfig tenant-service.kubeconfig auth can-i --list
   ```

   The NKE Controller must receive only the permissions in `nke:cwe`. Cluster
   API controller identities currently receive `cluster-admin`. A JWT with the
   wrong audience or an unbound `sub` must be rejected or denied.

6. Create a test ServiceAccount and narrowly scoped Role for tenant automation.
   Use a named ConfigMap so the allowed request needs only the `get` verb, then
   confirm that a cluster-scoped request outside the binding receives HTTP 403:

   ```bash
   VALIDATION_NAMESPACE="nke-api-access-validation-$(date +%s)"
   kubectl --kubeconfig "$TENANT_KUBECONFIG" \
     create namespace "$VALIDATION_NAMESPACE"
   kubectl --kubeconfig "$TENANT_KUBECONFIG" -n "$VALIDATION_NAMESPACE" \
     create serviceaccount reader
   kubectl --kubeconfig "$TENANT_KUBECONFIG" -n "$VALIDATION_NAMESPACE" \
     create configmap validation-target --from-literal=result=expected
   kubectl --kubeconfig "$TENANT_KUBECONFIG" -n "$VALIDATION_NAMESPACE" \
     create role configmap-reader --verb=get --resource=configmaps
   kubectl --kubeconfig "$TENANT_KUBECONFIG" -n "$VALIDATION_NAMESPACE" \
     create rolebinding configmap-reader --role=configmap-reader \
     --serviceaccount="$VALIDATION_NAMESPACE:reader"

   TENANT_TOKEN="$(kubectl --kubeconfig "$TENANT_KUBECONFIG" \
     -n "$VALIDATION_NAMESPACE" create token reader --duration=10m)"
   kubectl --server="$TENANT_API_SERVER" --certificate-authority="$TENANT_CA" \
     --token="$TENANT_TOKEN" -n "$VALIDATION_NAMESPACE" \
     get configmap validation-target -o name
   kubectl --server="$TENANT_API_SERVER" --certificate-authority="$TENANT_CA" \
     --token="$TENANT_TOKEN" get nodes

   kubectl --kubeconfig "$TENANT_KUBECONFIG" \
     delete namespace "$VALIDATION_NAMESPACE" --wait=true
   VALIDATION_NAMESPACE=
   unset TENANT_TOKEN
   ```

   The named ConfigMap request must succeed. The node-list request must fail
   with HTTP 403. The explicit deletion removes the test objects; the exit trap
   also requests namespace deletion if the shell exits early.

7. From a network that is intentionally not peered, routed, or connected by the
   approved VPN, confirm that the tenant endpoint is unreachable. Separately
   verify that no public DNS record or public load balancer exposes it.

8. If the cluster uses the first-control-plane-node fallback, repeat the
   endpoint inspection and record that the path is not highly available. Use a
   VIP-enabled cluster when validating the load-balanced endpoint model.

Human OIDC checks become applicable only after the identity-provider and RBAC
mapping contracts are implemented.
