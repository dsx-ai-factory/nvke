# DSX IMDS node-attestation fixture

This directory contains the built-in NVKE Tilt provider for local `dsx_imds`
node attestation. It owns the local IMDS and JWKS fixture plus the Tilt wiring
that builds the real SPIRE plugins from a neighboring `dsx-identity` checkout.

Clone `dsx-identity` beside the NVKE worktree, then enable the provider and set
the source checkout path in `tilt_config.json`:

```json
{
  "providers": [
    "capi_core",
    "bgp-containers",
    "capd",
    "nico",
    "dsx-imds-attestation"
  ],
  "dsx_imds_attestation_source_dir": "../../dsx-identity",
  "node_image_attestation_method": "dsxImds"
}
```

The provider builds both plugins, installs the agent plugin in the local CAPD
node image, deploys this fixture, configures the server plugin through the
SPIRE chart, and configures CWE's new-node attestation default and
metadata endpoint. Tilt's default attestation mode remains unchanged unless
the provider is enabled.

To exercise node-agent TLS/JWT too, add `dsx-identity-exchange` to the provider
list and set `site_controller_vip` to the management Kind node's IP reachable
from CAPD nodes. The exchange fixture validates the agent's SPIRE JWT-SVID for
`nvke-controller` and maps
`spiffe://<local-trust-domain>/tenant/nke/service/node-agent/bm/<node-UUID>` to
`spiffe://nke.capd.node-agent.nvid.id/bm/<same-node-UUID>`. The provider then
configures CWE to verify the fixture's ES256 JWTs and injects its HTTPS exchange
URL into node-agent bootstrap. Tilt installs the fixture's public CA in the
local node image so the agent verifies that endpoint's certificate.

Set `node_agent_source_dir` to an NVKE `agent/` checkout to build a specific
agent branch into the node image. This is useful for testing an agent PR without
changing the controller worktree. The option is local-only and does not affect
production node images.

For the full external-mTLS plus TLS/JWT path, set `dsx_imds_external_mtls` to
`true` alongside both providers. Tilt asks cert-manager to issue a test-only
controller client certificate with URI SAN `urn:nvidia:nke:controller`, waits
for its Secret, and enables `nodeAgent.externalMTLS` in CWE. Use an agent build
that supports `--controller-client-ca-bundle`; the controller writes this CA
bundle and flag into the generated node bootstrap files.
