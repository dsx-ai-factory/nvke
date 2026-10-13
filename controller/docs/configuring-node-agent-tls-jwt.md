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
SPIFFE trust domain and exact path `/bm/<node UUID>` for the node being contacted.
The controller allows gRPC traffic only after those checks succeed.

For TLS/JWT-enabled nodes, NVKE registers the local node-agent workload as
`spiffe://<local-trust-domain>/tenant/<subjectTenant>/service/<subjectService>/bm/<node UUID>`.
DSX Identity exchanges that canonical source identity into
`spiffe://<subjectTenant>.<region>.<subjectService>.<partition>/bm/<node UUID>`.
The configured `subjectTrustDomain` must equal that exchanged trust domain.
SPIFFE-only nodes that do not use DSX Identity exchange retain the legacy
`/host/<node UUID>` local identity.
