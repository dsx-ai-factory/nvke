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
| `clusterstaticentry-crd.yaml` | The CRD definition from the bootstrap cluster |
| `clusterstaticentries.yaml` | Sanitized workload registration resources |

The exported resources do not contain Kubernetes UIDs, resource versions,
status, SPIRE agent records, the SPIRE datastore, or CA private keys.

You also need:

- the `dsx-identity` Helm chart and its locked dependencies;
- a SPIRE server address and port that every host can reach;
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
```

Confirm that the command context and exported files are correct:

```bash
kubectl --kubeconfig "${SITE_KUBECONFIG}" get nodes
test -s "${BOOTSTRAP_ARTIFACT_DIR}/clusterstaticentry-crd.yaml"
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
Identity components only after you provide their required secrets, CRDs,
services, and values.

For a disposable test cluster without a default storage class, the verified
exercise used `persistence.type: emptyDir`. Do not use `emptyDir` for a
production server because a Pod replacement removes the SPIRE datastore.

## Install the CRD

Apply the CRD that bootstrap exported. Do this before Helm starts the SPIRE
controller manager.

```bash
kubectl --kubeconfig "${SITE_KUBECONFIG}" apply \
  -f "${BOOTSTRAP_ARTIFACT_DIR}/clusterstaticentry-crd.yaml"

kubectl --kubeconfig "${SITE_KUBECONFIG}" wait \
  --for=condition=Established \
  crd/clusterstaticentries.spire.spiffe.io \
  --timeout=60s
```

If the cluster already has this CRD, compare the served and storage versions
before you apply the exported definition. Do not replace a newer incompatible
CRD with an older definition.

## Stop an old in-cluster SPIRE controller

Skip this section when the site cluster does not run another SPIRE server or
controller manager.

Do not let two SPIRE controller managers process the same
`spire-mgmt-spire` class. Stop the old controller before the new controller
starts. Keep the old server, datastore, CA, and release available for rollback.

The exact scale command depends on the old release. Record its current replica
count and scale its server workload to zero. Do not delete the
`ClusterStaticEntry` CRD or the exported files.

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

## Publish a host-reachable SPIRE endpoint

The chart's default SPIRE Service is a `ClusterIP`. Host agents cannot use that
address. Publish the service through the site load balancer or another approved
host-reachable method.

The following NodePort service matches the verified test environment. Replace
the port when the site reserves another value.

```yaml
apiVersion: v1
kind: Service
metadata:
  name: dsx-identity-server-external
  namespace: dsx-identity
spec:
  type: NodePort
  selector:
    app.kubernetes.io/name: server
    app.kubernetes.io/instance: dsx-identity
  ports:
    - name: grpc
      protocol: TCP
      port: 8081
      targetPort: grpc
      nodePort: 30082
```

Apply the service and confirm that it has an endpoint:

```bash
kubectl --kubeconfig "${SITE_KUBECONFIG}" apply -f spire-endpoint.yaml
kubectl --kubeconfig "${SITE_KUBECONFIG}" \
  --namespace "${DSX_IDENTITY_NAMESPACE}" \
  get service dsx-identity-server-external
kubectl --kubeconfig "${SITE_KUBECONFIG}" \
  --namespace "${DSX_IDENTITY_NAMESPACE}" \
  get endpointslice \
  -l kubernetes.io/service-name=dsx-identity-server-external
```

In the verified Colossus exercise, agents used a control-plane node address and
NodePort `30082`. Production sites must use an address whose ownership and
routing survive a control-plane node failure.

## Restore the workload registrations

Apply the exported entries after the CRD and controller manager are ready:

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

## Point each existing host agent at the new server

Perform this step on every site host. Use the site's normal configuration
management system when one is available.

1. Back up the current agent configuration and public trust bundle.
2. Confirm that the configured trust bundle contains the CA certificate used
   for bootstrap. Do not replace it.
3. Set `server_address` to the published address.
4. Set `server_port` to the published port.
5. Keep the site-spec trust domain unchanged.
6. Set `rebootstrap_mode = "always"` so the agent can attest to the fresh
   server.
7. Restart the existing SPIRE agent service.

The relevant configuration must have this shape:

```hcl
agent {
  server_address = "<host-reachable-spire-address>"
  server_port = "<host-reachable-spire-port>"
  trust_domain = "<value-of-spire.trustDomain>"
  rebootstrap_mode = "always"
  trust_bundle_path = "/etc/spire-agent/trust-bundle.pem"
}
```

Do not change the TPM node-attestor plugin or its state during this procedure.

Restart and inspect the agent:

```bash
sudo systemctl restart spire-agent
sudo systemctl status spire-agent --no-pager
sudo journalctl --unit spire-agent --since -5m --no-pager
```

Use the actual unit name if the image uses another name. Restart `nke-agent`
after the SPIRE agent is healthy if `nke-agent` does not refresh its Workload
API connection:

```bash
sudo systemctl restart nke-agent
sudo journalctl --unit nke-agent --since -5m --no-pager
```

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
SPIFFE IDs must use `spire.trustDomain` from the site spec.

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

Remove the old SPIRE release only after every host has attested and obtained its
workload SVID from the DSX Identity server. Do not delete the shared CRD when
you remove the old release.

For rollback:

1. Stop the DSX Identity SPIRE controller manager if it conflicts with the old
   class owner.
2. Restore the old agent server address, port, and trusted CA bundle.
3. Restart the existing host SPIRE agents.
4. Confirm that agents and workloads reconnect to the old server.
5. Keep the exported CRD and entry files for the next cutover attempt.

Rollback is not possible after the old server state and CA have been removed
unless the site has a tested backup of both.

## Common failures

| Symptom | Check |
|---|---|
| Agent reports an unknown authority | The new server is not chaining to the CA bundle used for bootstrap, or the host bundle changed. Stop and compare the CA certificate digest. |
| Agent cannot connect | Confirm the published address, port, firewall, Service endpoints, and host-to-cluster routing. |
| TPM attestation is rejected | Confirm that the host TPM EK hash is in `nodeAttestor.tpmDirect.hashes`. |
| Entry exists but no workload SVID is issued | Confirm `spec.className`, `spec.parentID`, selectors, controller-manager logs, and agent attestation. |
| Controller ignores every exported entry | Set `controllerManager.className` to `spire-mgmt-spire`, or to the exact class in the exported objects. |
| Server loses registrations after a restart | Do not use `emptyDir`. Use a PVC or an external supported datastore. |
| `nke-agent` still uses an old certificate | Confirm the local agent is healthy, then restart `nke-agent` and inspect its journal. |
