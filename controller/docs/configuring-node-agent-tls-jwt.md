# Configuring node-agent TLS/JWT authentication

The controller can use the TLS/JWT handshake for every node-agent connection in
a deployment. This is a homogeneous, startup-time setting; it is not selected
or persisted per cluster or node. Do not change it on a controller that already
manages nodes using another transport mode.

TLS/JWT bootstrap is supported for both Nico profile-driven provisioning and
generic CAPI provisioning.

Enable it with the controller chart:

```yaml
nodeAgent:
  tlsJwt:
    enabled: true
    issuer: https://identity.example.com
    jwksURI: https://identity.example.com/.well-known/jwks.json
    algorithms:
      - RS256
    subjectTrustDomain: nke.example-site.node-agent.nvid.id
    subjectTenant: nke
    subjectService: node-agent
    dsxIdentityExchangeURL: https://identity.example.com/v1/exchange
```

The default is `false`, which preserves the existing node-agent transport and
does not add TLS/JWT configuration to the controller pod.

## Transport requirements

TLS/JWT authentication wraps a secure node-agent transport. The controller
uses external mTLS when configured, or SPIFFE mTLS for SPIFFE-enabled nodes.
At least one of those secure base transports must be enabled. The controller
validates the JWT sent by the agent before gRPC starts.

The chart supplies the issuer, JWKS URI, algorithm allow-list, subject trust
domain, local subject tenant and service, and DSX Identity exchange URL directly
as controller configuration.
It does not create or mount an
additional Secret or ConfigMap for this feature. The controller always requires
the JWT audience `nke-controller`.

## Node bootstrap and authentication

When enabled, controller-generated generic CAPI bootstrap data and rendered
Nico OS-profile user data write a complete `/etc/default/nvke-agent` file with
`--grpc-auth-mode=tls-jwt`, `--dsx-identity-exchange-url`, the secure server port, and the external mTLS CA
bundle flag when configured. CAPI bootstrap always restarts the agent after
writing its configuration file and running pre-kubeadm commands. Nico OS
profiles can render the controller-supplied file from `.Files` and must activate
the agent after it is written. The controller leaves the rendered profile data
unchanged when a profile omits or overrides the file.

The pre-gRPC authentication exchange has a fixed 15-second deadline, shortened
when the caller's context expires sooner. Successful handshakes clear that
deadline before handing the connection to gRPC.

For each connection, the controller presents its client certificate, reads the
agent's `NKEAuth` JWT record, and verifies its signature, issuer, fixed audience,
and required `exp`. When `nbf` is present, it is validated using the same clock
skew policy as API authentication. The verified `sub` must use the configured
SPIFFE trust domain and exact path `/bm/<node UUID>` or `/vm/<node UUID>` for the
node being contacted. `/vm/` is for a virtual machine whose node agent reads its
token from the DSX metadata-service sidecar, and whose SPIRE registration NVKE
creates with the node UUID. Such an agent has no SPIRE agent of its own, so it
must also use external mTLS (`--nvke-controller-client-ca-bundle`) for the
certificate it serves.
The controller allows gRPC traffic only after those checks succeed.

For TLS/JWT-enabled nodes, NVKE registers the local node-agent workload as
`spiffe://<local-trust-domain>/tenant/<subjectTenant>/service/<subjectService>/bm/<node UUID>`.
DSX Identity exchanges that canonical source identity into
`spiffe://<subjectTenant>.<region>.<subjectService>.<partition>/bm/<node UUID>`.
The configured `subjectTrustDomain` must equal that exchanged trust domain.
SPIFFE-only nodes that do not use DSX Identity exchange retain the legacy
`/host/<node UUID>` local identity.

## Nodes in virtual machines

CAPI nodes whose machine template is a CAPK `KubevirtMachineTemplate` run as
KubeVirt VMs. By default a VM node runs a SPIRE agent like a bare-metal node.
To have its node agent get its token from the DSX metadata service instead
(`--dsx-identity-metadata-url`), set `tokenFrom: metadataService` in the
control plane's or node pool's `nodeAgentConfig` block when you create it:

```json
"nodeAgentConfig": {
  "tokenFrom": "metadataService"
}
```

Such a node runs no SPIRE agent and gets no join token, so it needs TLS/JWT
with external mTLS. The setting can't be changed after creation.

For each VM node, `KubeVirtSpiffeController` creates these objects in the
cluster the VMs run in, and deletes them with the `KubevirtMachine`:

- A `LoadBalancer` Service in front of the VM, because its pod address is not
  reachable from the controller. The controller dials the agent there.
  The VM's `VirtualMachine` owns the Service, so it is also removed when the
  VM is. `nodeAgent.kubeVirt.addressPool` sets the MetalLB pool.
- For `metadataService` nodes, a ClusterSPIFFEID registering
  `spiffe://<local-trust-domain>/tenant/<subjectTenant>/service/<subjectService>/vm/<node UUID>`
  for the VM's pod. `nodeAgent.kubeVirt.spiffeIDClassName` must match the
  SPIRE controller manager serving the metadata sidecar.

Both select the pod by the node's UUID, which the controller labels the VM's
pod with, because CAPK's own pod labels carry the node name and node names are
reused.

`capi.rbac.kubeVirt: true` grants the controller these permissions, and read
access to `VirtualMachines`, in the management cluster. When the `KubevirtCluster` uses `infraClusterSecretRef`,
the identity in that kubeconfig needs them instead.

The VM template must request the sidecar and use `bridge` binding (the sidecar
does not work with `masquerade`):

```yaml
spec:
  template:
    spec:
      virtualMachineTemplate:
        spec:
          template:
            metadata:
              annotations:
                spiffe.nvidia.com/inject-dsx-imds: "true"
            spec:
              domain:
                devices:
                  interfaces:
                    - name: default
                      bridge: {}
              networks:
                - name: default
                  pod: {}
```
