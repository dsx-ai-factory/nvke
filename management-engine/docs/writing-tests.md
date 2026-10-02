# Writing Controller Tests

In these tests, terminal means a state that has settled for one fixture input,
including pending and failure states. Envtest cases are snapshots of that
state, not workflow scripts. Four rules follow from that.

- Use the existing create, update, or delete case set. Never add a case set for
  a flow that is specific to this provider.
- Group cases only when their settled generation differs.
- A step only waits for the case-set invariant, which is the current
  `observedGeneration`, the expected generation, or deletion. Goldens assert
  every field and condition.
- Use end-to-end tests for timing, watches, and interactions across
  controllers. Use envtest cases for individual state-machine states.

## Fixtures and Goldens

Two rules govern where golden state lives and how you regenerate it.

- Keep Kubernetes and NICo goldens together. `expected_objects.yaml` covers
  API-server state, and `expected_nico.yaml` covers HTTP requests and external
  resources.
- `make test-update` regenerates the goldens. Read the resulting diff before
  you commit it, because an unexpected change there usually means a behavior
  change.

## CAPNICo Generation Invariants

Each case set settles at a known generation, so an unexpected generation is a
signal rather than noise.

- `NicoCluster` create cases stay at generation 1, because reconciliation only
  writes status.
- A provisioned `NicoMachine` create case reaches generation 2 after CAPNICo
  writes `spec.providerID`.
- An unprovisioned `NicoMachine` create case stays at generation 1.
- A provisioned `NicoMachine` spec update reaches generation 3.

## Extending the Fake NICo Backend

`internal/fake`'s `Server` is a self-contained stand-in for the NICo API. Tests
run the production client against `Server.Handler()`, so no real NICo
deployment is needed. The package rule is to extend this HTTP surface instead
of bypassing serialization with an injected Go client. Three tasks cover almost
every extension.

To add an endpoint, register the route in `Handler()`, following the existing
method-and-path-to-handler pattern. For example, `GET
/v2/org/{org}/nico/instance/{instanceID}` maps to `s.getInstance`. Write the
handler as a method on `*Server`: lock `s.mu`, read or mutate state, unlock,
then respond with `writeJSON` or `writeError`.

To add seedable state, add a `Seed<Thing>(org string, thing nicosdk.<Thing>)`
method next to `SeedTenant`, `SeedInstanceType`, `SeedInstance`, `SeedMachine`,
`SeedSite`, and `SeedVPC`. Each method locks and clones the input. `SeedTenant`
stores tenants under `s.tenants[org]`. The other seed methods store resources in
maps keyed by `resourceKey(org, id)`. Add the matching map field to `Server`. If
fixtures should seed it from YAML, add a slice to `serverDump` and a case in
`SeedFromYAML`.

To add golden state, extend `Server.Dump()`. It marshals every resource map,
plus deduplicated write requests, to YAML, which is what `expected_nico.yaml`
compares against. Read polling is left out on purpose, so it cannot make
goldens flap. A new resource type needs a case in `Dump()` too, or it never
appears in the golden.
