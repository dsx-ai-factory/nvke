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
```

The default is `false`, which preserves the existing node-agent transport and
does not add TLS/JWT configuration to the controller pod.

## Transport requirements

TLS/JWT authentication wraps the controller's existing secure node-agent
transport. The current secure base transport is SPIFFE mTLS, so both the
controller deployment and the managed nodes must already have SPIFFE enabled.
The controller reuses its workload X.509 source for the TLS connection and then
validates the JWT sent by the agent before gRPC starts.

The chart supplies the issuer, JWKS URI, algorithm allow-list, and subject trust
domain directly as controller configuration. It does not create or mount an
additional Secret or ConfigMap for this feature. The controller always requires
the JWT audience `nke-controller`.

## Node bootstrap and authentication

When enabled, controller-generated generic CAPI bootstrap data and rendered
Nico OS-profile user data:

- remove `--disable-mtls` and any existing `--grpc-auth-mode` value;
- add `--grpc-auth-mode=tls-jwt` to the node-agent arguments; and
- restarts `nke-agent.service` after applying the setting.

The pre-gRPC authentication exchange has a fixed 15-second deadline, shortened
when the caller's context expires sooner. Successful handshakes clear that
deadline before handing the connection to gRPC.

For each connection, the controller presents its client certificate, reads the
agent's `NKEAuth` JWT record, and verifies its signature, issuer, fixed audience,
and required `exp`. When `nbf` is present, it is validated using the same clock
skew policy as API authentication. The verified `sub` must use the configured
SPIFFE trust domain and exact path `/bm/<node UUID>` for the node being contacted.
The controller allows gRPC traffic only after those checks succeed.
