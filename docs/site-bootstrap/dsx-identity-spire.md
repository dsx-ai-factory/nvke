# Install DSX Identity SPIRE After Site Bootstrap

This runbook installs the SPIRE server from the `dsx-identity` Helm chart on a
site controller cluster. It then restores the `ClusterStaticEntry` resources
that `nkectl bootstrap site` exported from the temporary bootstrap cluster.

This procedure keeps the existing SPIRE agent on each host. Do not install a
second SPIRE agent.

## Result

At the end of this procedure:

- the site controller cluster runs the DSX Identity SPIRE server;
- the new server uses the bootstrap server's SPIFFE trust domain and upstream
  CA trust bundle;
- the SPIRE controller manager has the same class name that bootstrap used;
- each host agent connects to the new server endpoint and attests again;
- the exported workload registrations exist as `ClusterStaticEntry` objects;
- `nke-agent` can obtain its workload SVID from the existing host agent.

The `ClusterStaticEntry` resources are registration policy. They are not SPIRE
agent records. A fresh SPIRE server creates new agent records when the host
agents attest.

## Inputs

`nkectl bootstrap site` writes these files in the directory that contains the
site kubeconfig:

| File | Purpose |
|---|---|
| `<cluster>.kubeconfig` | Administrator access to the site controller cluster |
| `clusterstaticentries.yaml` | Sanitized workload registration resources |

The exported resources do not contain Kubernetes UIDs, resource versions,
status, SPIRE agent records, the SPIRE datastore, or CA private keys.

You also need:

- the `dsx-identity` Helm chart and its locked dependencies;
- the SPIRE VIP and port already configured on every host agent;
- the TPM endorsement-key hash for each host;
- the site spec used for bootstrap;
- the CA certificate and private key named by `spire.caCertFile` and
  `spire.caKeyFile` in that site spec.

The site spec must set `spire.trustDomain`, `spire.caCertFile`, and
`spire.caKeyFile`. Stop if the bootstrap used a self-signed SPIRE CA or if the
original CA material is not available. A change of CA or trust domain is an
identity migration and is outside this runbook.

Use a supported Kubernetes version for the selected `dsx-identity` chart
release. The current chart requirements are in the chart README.

The DSX Identity chart must install a compatible `ClusterStaticEntry` CRD. Do
not use the CRD exported from the bootstrap Kind cluster as an installation
step. If the selected chart does not include the CRD, fix or replace the chart
before you continue.

The bootstrap Kind cluster normally exists only for the bootstrap. For the
first cutover at a site, run bootstrap with `--keep-bootstrap-cluster`. Keep
that cluster intact until the DSX Identity server, agent attestation, workload
SVIDs, and authenticated CWE calls are validated.

## Set command variables

Run all Kubernetes and Helm commands from a trusted administration host.

```bash
export SITE_KUBECONFIG=/secure/path/site.kubeconfig
export BOOTSTRAP_ARTIFACT_DIR="$(dirname "${SITE_KUBECONFIG}")"
export DSX_IDENTITY_NAMESPACE=dsx-identity
export DSX_IDENTITY_RELEASE=dsx-identity
export DSX_IDENTITY_CHART=/path/to/dsx-identity/charts/dsx-identity
export DSX_IDENTITY_VALUES=/secure/path/dsx-identity-values.yaml
export SITE_SPEC=/secure/path/inventory.yaml
export SPIRE_TRUST_DOMAIN=<value-of-spire.trustDomain>
export SPIRE_CA_CERT_FILE=<value-of-spire.caCertFile>
export SPIRE_CA_KEY_FILE=<value-of-spire.caKeyFile>
export SPIRE_VIP=<same-vip-already-configured-on-host-agents>
export SPIRE_PORT=<same-port-already-configured-on-host-agents>
export BOOTSTRAP_KIND_CLUSTER=<bootstrap-kind-cluster-name>
```

Confirm that the command context and exported files are correct:

```bash
kubectl --kubeconfig "${SITE_KUBECONFIG}" get nodes
test -s "${BOOTSTRAP_ARTIFACT_DIR}/clusterstaticentries.yaml"
test -s "${SITE_SPEC}"
test -s "${SPIRE_CA_CERT_FILE}"
test -s "${SPIRE_CA_KEY_FILE}"
```

Keep the exported files until the cutover and rollback period are complete.
They contain identity policy and TPM-derived parent IDs. Store them with the
same access controls as the site kubeconfig.

## Preserve the trust domain and bundle

The SPIFFE trust domain and the CA trust bundle are different settings. The new
server must use both original values:

- `global.spire.trustDomain` must equal `spire.trustDomain` from the site spec;
- the disk upstream authority must use the certificate and key from
  `spire.caCertFile` and `spire.caKeyFile`;
- `bundle.crt` must contain the same certificate that bootstrap used as its
  upstream trust bundle.

`nkectl bootstrap site` supplies the CA certificate as both the upstream
certificate and its bundle. The replacement server must do the same. It can
create a new intermediate signing CA, but the published chain still terminates
at the CA that the host agents already trust.

Record the public certificate digest before installation:

```bash
openssl x509 -in "${SPIRE_CA_CERT_FILE}" -noout -subject -issuer -fingerprint -sha256
```

Create the namespace and upstream-CA Secret through the approved
secret-management system. This command shows the required Secret keys:

```bash
kubectl --kubeconfig "${SITE_KUBECONFIG}" create namespace \
  "${DSX_IDENTITY_NAMESPACE}" --dry-run=client -o yaml | \
  kubectl --kubeconfig "${SITE_KUBECONFIG}" apply -f -

kubectl --kubeconfig "${SITE_KUBECONFIG}" \
  --namespace "${DSX_IDENTITY_NAMESPACE}" create secret generic dsx-identity-ca \
  --from-file=tls.crt="${SPIRE_CA_CERT_FILE}" \
  --from-file=tls.key="${SPIRE_CA_KEY_FILE}" \
  --from-file=bundle.crt="${SPIRE_CA_CERT_FILE}" \
  --dry-run=client -o yaml | \
  kubectl --kubeconfig "${SITE_KUBECONFIG}" apply -f -
```

Do not put the private key in a Helm values file or in source control. Do not
replace the host trust bundle during this procedure.

## Prepare the DSX Identity values

Create a values file based on this minimal SPIRE-only profile. Replace every
value in angle brackets. Keep image and chart versions pinned.

```yaml
global:
  operatorTenant: <operator-tenant>
  dsx:
    jwtIssuer: https://identity.<site-domain>
    region: <region>
    partition: <partition>
    defaultServiceName: dsx
  spire:
    # This must match the trust domain in the exported SPIFFE IDs and host
    # agent configuration.
    trustDomain: <value-of-spire.trustDomain>
    clusterName: <site-cluster-name>
    jwtIssuer: https://spire.<site-domain>

spire:
  spire-server:
    persistence:
      type: pvc
      size: 1Gi
      storageClass: <storage-class>
    tolerations:
      - key: node-role.kubernetes.io/control-plane
        operator: Exists
        effect: NoSchedule
    controllerManager:
      enabled: true
      # This must match spec.className in the exported entries.
      className: spire-mgmt-spire
      extraEnv:
        - name: OPERATOR_TENANT
          value: <operator-tenant>
    nodeAttestor:
      k8sPSAT:
        enabled: false
      tpmDirect:
        enabled: true
        hashes:
          - <host-1-tpm-ek-hash>
          - <host-2-tpm-ek-hash>
          - <host-3-tpm-ek-hash>
    upstreamAuthority:
      disk:
        enabled: true
        secret:
          create: false
          name: dsx-identity-ca
  spire-agent:
    # The existing host service supplies the agent.
    enabled: false
  spiffe-csi-driver:
    enabled: false
  spiffe-oidc-discovery-provider:
    enabled: false

observability:
  metrics:
    enabled: false
    serviceMonitor:
      enabled: false

identityKeys:
  enabled: false
podTokenFetcher:
  enabled: false
identityController:
  enabled: false
dpuService:
  tokenFetcher:
    enabled: false
  imds:
    enabled: false
  spiffeCsi:
    enabled: false
vmSupport:
  enabled: false
kataSupport:
  enabled: false
```

This profile is for the SPIRE replacement exercise. Enable the other DSX
Identity components only after you provide their required secrets, services,
and values.

For a disposable test cluster without a default storage class, the verified
exercise used `persistence.type: emptyDir`. Do not use `emptyDir` for a
production server because a Pod replacement removes the SPIRE datastore.

## Install DSX Identity

When you install from source, build the locked chart dependency first:

```bash
helm dependency build "${DSX_IDENTITY_CHART}"
```

Install the chart:

```bash
helm upgrade --install "${DSX_IDENTITY_RELEASE}" "${DSX_IDENTITY_CHART}" \
  --kubeconfig "${SITE_KUBECONFIG}" \
  --namespace "${DSX_IDENTITY_NAMESPACE}" \
  --create-namespace \
  --values "${DSX_IDENTITY_VALUES}" \
  --atomic \
  --timeout 15m
```

Wait for the server StatefulSet:

```bash
kubectl --kubeconfig "${SITE_KUBECONFIG}" \
  --namespace "${DSX_IDENTITY_NAMESPACE}" \
  rollout status statefulset/dsx-identity-spire-server \
  --timeout=10m
```

The SPIRE controller manager runs in the server Pod for this chart. Confirm
that both containers are ready:

```bash
kubectl --kubeconfig "${SITE_KUBECONFIG}" \
  --namespace "${DSX_IDENTITY_NAMESPACE}" \
  get pods -l app.kubernetes.io/component=server
```

Confirm that the chart installed its `ClusterStaticEntry` CRD. Stop if the CRD
is absent. Do not apply the copy exported from the bootstrap cluster.

```bash
kubectl --kubeconfig "${SITE_KUBECONFIG}" wait \
  --for=condition=Established \
  crd/clusterstaticentries.spire.spiffe.io \
  --timeout=60s
```

## Publish a host-reachable SPIRE endpoint

Host agents must continue to use the SPIRE VIP and port that they received
before bootstrap. Do not change their configuration during the cutover.

Deploy kube-vip, or the site's equivalent VIP manager, on the site cluster.
Configure it to manage `${SPIRE_VIP}` in service mode and to advertise that
address with the site's supported L2 or BGP method. Use a pinned, approved
kube-vip manifest. The address must be reserved for SPIRE and must not be
allocated to another Service.

Only one cluster can advertise the VIP. If the bootstrap Kind cluster was
retained, withdraw its VIP advertisement immediately before the site cluster
starts to advertise the address. Keep the Kind cluster and its SPIRE state
intact so you can restore its advertisement for rollback.

Create a `LoadBalancer` Service that requests the existing SPIRE VIP. If the
selected VIP manager uses an annotation instead of `loadBalancerIP`, use its
documented equivalent.

```yaml
apiVersion: v1
kind: Service
metadata:
  name: dsx-identity-server-external
  namespace: dsx-identity
spec:
  type: LoadBalancer
  loadBalancerIP: <same-spire-vip>
  selector:
    app.kubernetes.io/name: server
    app.kubernetes.io/instance: dsx-identity
  ports:
    - name: grpc
      protocol: TCP
      port: <same-spire-port>
      targetPort: grpc
```

Substitute the configured VIP and port, apply the Service, and confirm that the
VIP manager assigned the exact requested address:

```bash
sed -e "s/<same-spire-vip>/${SPIRE_VIP}/" \
  -e "s/<same-spire-port>/${SPIRE_PORT}/" \
  spire-endpoint.yaml | \
  kubectl --kubeconfig "${SITE_KUBECONFIG}" apply -f -
kubectl --kubeconfig "${SITE_KUBECONFIG}" \
  --namespace "${DSX_IDENTITY_NAMESPACE}" \
  get service dsx-identity-server-external \
  -o wide
kubectl --kubeconfig "${SITE_KUBECONFIG}" \
  --namespace "${DSX_IDENTITY_NAMESPACE}" \
  get endpointslice \
  -l kubernetes.io/service-name=dsx-identity-server-external
test "$(kubectl --kubeconfig "${SITE_KUBECONFIG}" \
  --namespace "${DSX_IDENTITY_NAMESPACE}" \
  get service dsx-identity-server-external \
  -o jsonpath='{.status.loadBalancer.ingress[0].ip}')" = "${SPIRE_VIP}"
```

The Service `EXTERNAL-IP` must equal `${SPIRE_VIP}`. From a host network, test
TCP connectivity to `${SPIRE_VIP}:${SPIRE_PORT}`. The VIP manager must move or
re-advertise the address after a control-plane node failure.

## Restore the workload registrations

Apply the exported entries after the chart-provided CRD and controller manager
are ready:

```bash
kubectl --kubeconfig "${SITE_KUBECONFIG}" apply \
  -f "${BOOTSTRAP_ARTIFACT_DIR}/clusterstaticentries.yaml"

kubectl --kubeconfig "${SITE_KUBECONFIG}" \
  get clusterstaticentries.spire.spiffe.io
```

Confirm that the controller accepts the configured class and registers the
entries:

```bash
kubectl --kubeconfig "${SITE_KUBECONFIG}" \
  --namespace "${DSX_IDENTITY_NAMESPACE}" \
  logs statefulset/dsx-identity-spire-server \
  --container spire-controller-manager \
  --tail=200
```

Do not copy `status`, Kubernetes UIDs, resource versions, or SPIRE entry IDs
from the old cluster. The exported file intentionally omits them.

## Verify attestation and workload SVIDs

List the newly attested agents from the server Pod:

```bash
kubectl --kubeconfig "${SITE_KUBECONFIG}" \
  --namespace "${DSX_IDENTITY_NAMESPACE}" \
  exec statefulset/dsx-identity-spire-server \
  --container spire-server -- \
  /opt/spire/bin/spire-server agent list
```

The list must contain one active agent for each expected TPM hash. The agent
SPIFFE IDs must use `spire.trustDomain` from the site spec. The agents must
reconnect and re-attest without a configuration change because the VIP, port,
trust domain, and public trust bundle did not change.

On each host, ask the local Workload API for the root-owned workload SVID:

```bash
sudo spire-agent api fetch x509 \
  -socketPath /run/spire/agent-sockets/spire-agent.sock
```

Confirm all of these conditions:

- the host agent attested to the DSX Identity server;
- the returned workload SVID has the expected host SPIFFE ID;
- `nke-agent` has no Workload API, certificate, or authorization errors;
- the `ClusterStaticEntry` resources report no reconciliation errors;
- CWE can make an authenticated gRPC request to each `nke-agent` after CWE is
  deployed on the site cluster.

## Complete or roll back the cutover

For the first bootstrap at a site, preserve the bootstrap Kind cluster with
`--keep-bootstrap-cluster`. Do not delete it merely because the DSX Identity
server started. Delete it only after every host has attested, every required
workload has obtained its SVID, and CWE has completed authenticated calls to
all `nke-agent` instances.

After successful validation, delete the retained cluster with the same runtime
provider used to create it:

```bash
KIND_EXPERIMENTAL_PROVIDER=nerdctl kind delete cluster \
  --name "${BOOTSTRAP_KIND_CLUSTER}"
```

For rollback:

1. Withdraw `${SPIRE_VIP}` from the site cluster. Stop its kube-vip Service
   advertisement before the old environment advertises it.
2. Restore the bootstrap Kind cluster's advertisement of the same VIP.
3. Confirm that the VIP resolves to the bootstrap SPIRE server.
4. Confirm that agents and workloads reconnect without configuration changes.
5. Keep the exported entry file for the next cutover attempt.

After the bootstrap Kind cluster is deleted, rollback requires a tested backup
of its SPIRE state and the ability to restore its VIP advertisement.

## Common failures

| Symptom | Check |
|---|---|
| Agent reports an unknown authority | The new server is not chaining to the CA bundle used for bootstrap, or the host bundle changed. Stop and compare the CA certificate digest. |
| Agent cannot connect | Confirm the VIP and port, firewall, Service endpoints, host-to-cluster routing, and kube-vip advertisement. |
| VIP moves between old and new servers | Both clusters advertise the address. Withdraw one advertisement before enabling the other. |
| TPM attestation is rejected | Confirm that the host TPM EK hash is in `nodeAttestor.tpmDirect.hashes`. |
| Entry exists but no workload SVID is issued | Confirm `spec.className`, `spec.parentID`, selectors, controller-manager logs, and agent attestation. |
| Controller ignores every exported entry | Set `controllerManager.className` to `spire-mgmt-spire`, or to the exact class in the exported objects. |
| Server loses registrations after a restart | Do not use `emptyDir`. Use a PVC or an external supported datastore. |
