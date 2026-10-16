# Tenant Kubernetes API Access

## Overview

NVKE exposes each tenant cluster's Kubernetes API on the endpoint recorded in
that cluster's kubeconfig. Clients connect to that endpoint directly; neither
the NVKE Controller nor `nvkectl` proxies Kubernetes API traffic.

The preferred endpoint is a private kube-vip address shared by the control-plane
nodes. If the cluster has no control-plane VIP, NVKE uses the first
control-plane node's IP address instead. That fallback is not load balanced and
loses API availability when that node or its network path is unavailable.

NVKE does not create a public Kubernetes API endpoint. A deployment must provide
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
| Tenant operator | The operator obtains an `nvke:breakglass` kubeconfig with `nvkectl get kubeconfig`. It contains an X.509 client certificate and private key. | The certificate's organization is `nvke:breakglass`, which is bound to `cluster-admin`. | The operator connects directly to the tenant API endpoint from an approved private network. | One year. See [Alpha Operator Access](#alpha-operator-access) for renewal and retrieval details. |
| Tenant user | NVKE alpha does not deliver a distinct, individually attributable tenant-user credential. A tenant operator must establish the user's authentication outside NVKE. | A tenant operator creates the required Kubernetes RBAC bindings. NVKE grants no default tenant-user role. | The user connects directly to the tenant API endpoint from an approved private network. | Determined by the tenant's chosen authentication mechanism. |
| Tenant automation | A tenant operator creates a Kubernetes ServiceAccount and uses a TokenRequest token, or configures another Kubernetes-supported authenticator. NVKE does not distribute a separate automation credential. | A tenant operator binds the ServiceAccount or other identity to a workload-specific Role or ClusterRole. NVKE grants no default role. | The automation connects directly to the tenant API endpoint, normally from the tenant network or from inside the cluster. | TokenRequest tokens are time limited. Their requested and maximum lifetimes are deployment policy rather than an NVKE tenant-access setting. |
| NVKE Controller | The NVKE Controller's SPIFFE workload identity obtains a short-lived JWT-SVID, optionally exchanges it through the configured token-exchange service, and presents the resulting bearer token to the tenant API server. | The token's `sub` claim maps to a Kubernetes user. NVKE binds that user to the `nvke:cwe` ClusterRole and namespace-scoped Roles needed for lifecycle operations. | The NVKE Controller connects from the Site Controller cluster directly to the tenant API endpoint over the site's private network. | JWT expiry is set by the issuing identity system. The client obtains a new token rather than storing a long-lived bearer token. |
| Cluster API controllers | Each controller uses its SPIFFE workload identity and the same JWT authentication path as the NVKE Controller. | The current NVKE manifest binds the CAPI manager identities to `cluster-admin`. | The controllers connect from the Site Controller cluster directly to the tenant API endpoint. | JWT expiry is set by the issuing identity system, and clients obtain replacement tokens as needed. |
| Break-fix system | NVKE alpha defines no generic break-fix credential. A break-fix deployment must use a dedicated ServiceAccount or workload identity rather than the operator admin kubeconfig. | The deployer creates RBAC matching the repair operations the system performs. NVKE grants no default break-fix role. | The system connects directly to the private tenant API endpoint from its approved runtime network. | Determined by the selected ServiceAccount or workload-identity mechanism. |
| Break-glass operator | The operator obtains the same `nvke:breakglass` kubeconfig used by the alpha tenant-operator path. Access to the management-cluster Secret is the delivery control. | `cluster-admin`, through the `nvke:breakglass` group. | The operator must have both management-cluster access to retrieve the kubeconfig and private network reachability to the tenant API endpoint. | The same one-year certificate and renewal behavior apply. The exported copy does not refresh or gain the renewed certificate automatically. Treat every downloaded copy as sensitive until its certificate expires or the cluster CA and credentials are replaced. |

The overlap between routine tenant-operator access and break-glass access is an
alpha limitation: both use the same highly privileged, non-attributable
credential. Do not share this kubeconfig with applications or commit it to a
repository.

## Alpha Operator Access

An operator first configures `nvkectl` with a kubeconfig for the Site Controller
cluster. That management-cluster identity must be allowed to read the tenant
access Secret in `nvke-system`.

```bash
nvkectl config set-site --name "$SITE_NAME" \
  --url "$NKE_CONTROLLER_URL" \
  --kubeconfig site-controller.kubeconfig

nvkectl get kubeconfig "$CLUSTER_NAME" --output-file tenant.kubeconfig
kubectl --kubeconfig tenant.kubeconfig get nodes
```

`nvkectl get kubeconfig` reads
`nvke-system/<cluster>-admin-kubeconfig` from the Site Controller cluster. The
Secret contains an `nvke:breakglass` client certificate. The server URL and
cluster CA come from a control-plane node's `admin.conf`; the client key does
not. The resulting `kubectl` process then connects directly to the server URL
inside that kubeconfig.

Retrieving the kubeconfig proves access to the management-cluster Secret; it
does not prove that the operator's workstation can route to the tenant API
endpoint.

The `nvke:breakglass` client certificate is valid for one year. Tenant-access
checks it whenever it reconciles the cluster, which the database resync does
about every 15 minutes. When the remaining lifetime is less than
`scheduledMaintenance.certRenewal.thresholdDays` (30 by default), it submits a
certificate signing request named `nvke-breakglass` and approves it.
kube-controller-manager issues the certificate, and tenant-access replaces the
Secret. The node's `admin.conf` is used to approve the request and to copy the
server URL and cluster CA. That `admin.conf` is kept valid by the separate
12-hour kubeadm certificate renewal. Renewal updates the Secret, not kubeconfig
files that operators have already downloaded. Operators must retrieve a
refreshed kubeconfig before the certificate in their local copy expires.

## Service Identity and Tenant OIDC

NVKE's existing SPIFFE/OIDC integration is for service-to-cluster API access. It
is separate from the alpha human-operator flow.

For an OIDC-enabled tenant cluster, the tenant API server uses Kubernetes
structured authentication configuration. It validates the configured issuer,
signature, audience, and expiry and maps the JWT `sub` claim to the Kubernetes
username. Kubernetes RBAC then authorizes that username. The OIDC discovery
provider publishes discovery metadata and signing keys; it does not grant
permissions.

The NVKE Controller publishes a separate `nvke-system/<cluster>-kubeconfig` for
its own use. On an OIDC-enabled cluster, this kubeconfig runs the SPIFFE
exec-auth plugin to obtain a JWT. It is not the kubeconfig returned by
`nvkectl get kubeconfig`.

## Authorization Model

Authentication establishes a Kubernetes username and, where applicable,
groups. Kubernetes RBAC makes the separate authorization decision:

- The alpha admin certificate is a client in the `nvke:breakglass` group,
  which is bound to `cluster-admin`.
- The NVKE Controller's JWT `sub` maps to the user named in NVKE's `nvke:cwe`
  bindings. The identifier is retained in the current RBAC manifest. Its
  ClusterRole and namespace Roles enumerate the lifecycle operations the NVKE
  Controller may perform.
- The CAPI manager JWT subjects are currently bound to `cluster-admin`.
- Tenant users, tenant automation, and break-fix systems receive no additional
  default NVKE role. A tenant administrator must create explicit bindings for
  them.

RBAC authorization does not make an endpoint reachable, and network
reachability does not grant Kubernetes permissions. Both checks must succeed.

## Planned Human OIDC Access

After alpha, tenant operators and users are expected to authenticate through an
approved identity provider instead of sharing the `nvke:breakglass` kubeconfig. The intended flow
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

The NVKE Controller and the Cluster API controllers connect from the Site
Controller cluster to the private tenant API endpoint. This path is required
for cluster lifecycle and reconciliation. The Site Controller deployment must
be able to route to TCP port 6443 on the endpoint.

### In-VPC access

A client already on a network with a route to the tenant endpoint can connect
directly to TCP port 6443. TLS authenticates the API server, and the credentials
in the request authenticate the client.

### Tenant peering or VPN

An operator outside the directly connected network needs an approved private
path, such as tenant peering or a VPN. NVKE records and configures the Kubernetes
API endpoint but does not, by itself, establish the operator's peering, VPN,
firewall, or DNS configuration.

### Public networks

NVKE does not provision a public load balancer or public ingress for a tenant
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

   nvkectl get kubeconfig "$CLUSTER_NAME" --output-file "$TENANT_KUBECONFIG"
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

   The alpha admin kubeconfig must reach the API, identify a client in the
   `nvke:breakglass` group, and report that all actions are allowed.

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

   The NVKE Controller must receive only the permissions in `nvke:cwe`. Cluster
   API controller identities currently receive `cluster-admin`. A JWT with the
   wrong audience or an unbound `sub` must be rejected or denied.

6. Create a test ServiceAccount and narrowly scoped Role for tenant automation.
   Use a named ConfigMap so the allowed request needs only the `get` verb, then
   confirm that a cluster-scoped request outside the binding receives HTTP 403:

   ```bash
   VALIDATION_NAMESPACE="nvke-api-access-validation-$(date +%s)"
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

## Tenant Kubernetes API Access Through a Bastion

When the tenant API endpoint is not routable from your workstation but is
routable from a bastion host, use one of the two approaches below. Both
approaches:

- Fetch the tenant kubeconfig from the Site Controller cluster.
- Require SSH access to the bastion but not to a tenant control-plane node.
- Require the bastion to have network connectivity to the tenant API endpoint.

NVKE does not create or configure the bastion, and it does not distribute SSH
keys for access to it.

| | Option A: `kubectl` on your laptop | Option B: `kubectl` on the bastion |
|---|---|---|
| Kubeconfig location | Your laptop | The bastion |
| Network path | SSH tunnel from the laptop to the bastion, then direct TCP from the bastion to the tenant API endpoint | SSH session on the bastion, then direct TCP from the bastion to the tenant API endpoint |
| Kubeconfig modifications | Set the server endpoint to `localhost` and set `tls-server-name` to the original endpoint | None |
| Required access | Bastion SSH access with TCP forwarding, Site Controller access from your laptop | Bastion SSH access with a shell, Site Controller access from the bastion or from your laptop |

### Option A: `kubectl` on your laptop

1. Fetch the kubeconfig, as described in
   [Alpha Operator Access](#alpha-operator-access):

   ```bash
   nvkectl get kubeconfig "$CLUSTER_NAME" --output-file tenant.kubeconfig
   chmod 0600 tenant.kubeconfig
   ```

2. In a dedicated terminal, read the tenant API endpoint from the kubeconfig,
   then open a tunnel that forwards a local port to it through the bastion.
   Leave the tunnel running:

   ```bash
   TENANT_API_SERVER="$(kubectl --kubeconfig tenant.kubeconfig \
     config view --minify -o jsonpath='{.clusters[0].cluster.server}')"
   TENANT_API_HOSTPORT="${TENANT_API_SERVER#https://}"

   ssh -N -o ExitOnForwardFailure=yes \
     -L "localhost:16443:${TENANT_API_HOSTPORT}" "$BASTION_USER@$BASTION_HOST"
   ```

3. In a second terminal, point the kubeconfig at the tunnel. The API server
   certificate does not list `localhost` among its SANs, so keep the CA data
   and set `tls-server-name` to the original endpoint host, which is in the
   SANs:

   ```bash
   CLUSTER_ENTRY="$(kubectl --kubeconfig tenant.kubeconfig \
     config view --minify -o jsonpath='{.clusters[0].name}')"
   TENANT_API_HOST="$(kubectl --kubeconfig tenant.kubeconfig \
     config view --minify -o jsonpath='{.clusters[0].cluster.server}')"
   TENANT_API_HOST="${TENANT_API_HOST#https://}"
   TENANT_API_HOST="${TENANT_API_HOST%:*}"

   kubectl --kubeconfig tenant.kubeconfig config set-cluster "$CLUSTER_ENTRY" \
     --server=https://localhost:16443 --tls-server-name="$TENANT_API_HOST"
   ```

4. Run `kubectl` commands in the second terminal:

   ```bash
   kubectl --kubeconfig tenant.kubeconfig get nodes
   ```

5. When you finish your `kubectl` session, stop the tunnel with Ctrl-C in
   the first terminal and remove the kubeconfig from your laptop:

   ```bash
   rm -f tenant.kubeconfig
   ```

### Option B: `kubectl` on the bastion

1. Copy the kubeconfig to the bastion.

   If the bastion can reach the Site Controller cluster, SSH to the bastion and
   fetch the kubeconfig there, as described in
   [Alpha Operator Access](#alpha-operator-access):

   ```bash
   ssh "$BASTION_USER@$BASTION_HOST"
   nvkectl get kubeconfig "$CLUSTER_NAME" --output-file tenant.kubeconfig
   chmod 0600 tenant.kubeconfig
   ```

   Otherwise, fetch the kubeconfig on your laptop and copy it to the bastion.
   Set the mode after copying, because `scp` keeps the mode of an existing
   destination file:

   ```bash
   nvkectl get kubeconfig "$CLUSTER_NAME" --output-file tenant.kubeconfig
   scp tenant.kubeconfig "${BASTION_USER}@${BASTION_HOST}:tenant.kubeconfig"
   ssh "$BASTION_USER@$BASTION_HOST" 'chmod 0600 tenant.kubeconfig'
   ```

2. If you are not already connected, SSH to the bastion:

   ```bash
   ssh "$BASTION_USER@$BASTION_HOST"
   ```

3. Run `kubectl` commands on the bastion. The kubeconfig needs no
   modifications because the tenant API endpoint is included in the SANs of the
   API server certificate:

   ```bash
   kubectl --kubeconfig tenant.kubeconfig get nodes
   ```

4. When you finish your `kubectl` session, remove the kubeconfig from the
   bastion:

   ```bash
   rm -f tenant.kubeconfig
   ```

   If you copied the kubeconfig to the bastion from your laptop, also remove
   the copy on your laptop.
