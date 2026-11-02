# CAPD DSX identity exchange test environment

This directory contains a development-only identity service used by the focused
CAPD end-to-end lane. It exercises the same boundary as the DSX identity
exchange without depending on a deployed DSX environment:

1. a management-cluster workload obtains a real JWT-SVID from the local SPIRE
   server;
2. the mock validates that JWT-SVID against SPIRE's current JWT bundle;
3. the mock rewrites the workload's local SPIFFE ID into the DSX identity
   shape and signs a replacement ES256 JWT; and
4. a real CAPD tenant apiserver discovers the mock's JWKS and authenticates the
   replacement JWT.

The mock is test infrastructure. It is not built into the controller image and
must not be deployed as a production identity service.

## Environment

The lane creates a Kind management cluster and a one-control-plane CAPD tenant
cluster. CWE, the CAPI controllers, SPIRE, and the exchange mock run in the
management cluster. The tenant apiserver is configured with
`https://capd.dsx.nvid.id` as its logical issuer and reaches OIDC discovery and
JWKS through the mock's HTTPS NodePort.

```text
Management Kind cluster

  CWE / CAPI / KCP / CAPD workload
              |
              | 1. fetch a JWT-SVID through the SPIRE Workload API
              v
        SPIRE Agent and Server
              |
              | 2. POST the JWT-SVID using RFC 8693 token exchange
              v
    dsx-identity-exchange-mock
       |                    |
       | validates source   | signs replacement JWT
       | against SPIRE      | and publishes OIDC/JWKS
       v                    v
  local SPIRE bundle      HTTPS NodePort :30444
                                  |
                                  | 3. discovery and signature verification
                                  v
                         CAPD tenant apiserver
                                  |
                                  | 4. username and RBAC authorization
                                  v
                          real tenant API request
```

There are two distinct trust relationships:

- The exchange mock trusts source identities only after validating their
  JWT-SVID signature and expiry through the SPIRE Workload API bundle source.
- CWE, the CAPI controllers, verifier pods, and the tenant apiserver trust the
  mock's HTTPS certificate through the test CA distributed by the Tilt
  provider. The tenant apiserver separately verifies replacement JWT signatures
  with the public key advertised by OIDC discovery.

## Identity mapping

The local workload identity has this form:

```text
spiffe://<kind-cluster>.nvke.local/tenant/nke/service/<service-account>/bm/cwe
```

The mock maps it to:

```text
spiffe://nke.capd.<service-account>.nvid.id/bm/cwe
```

The E2E lane verifies these concrete mappings:

| Workload | Tenant username |
|---|---|
| Controller | `spiffe://nke.capd.nvke-controller.nvid.id/bm/cwe` |
| CAPI manager | `spiffe://nke.capd.capi-manager.nvid.id/bm/cwe` |
| Kubeadm control-plane manager | `spiffe://nke.capd.capi-kubeadm-control-plane-manager.nvid.id/bm/cwe` |
| Kubeadm bootstrap manager | `spiffe://nke.capd.capi-kubeadm-bootstrap-manager.nvid.id/bm/cwe` |
| CAPD manager | `spiffe://nke.capd.capd-manager.nvid.id/bm/cwe` |

`region=capd` and `partition=nvid.id` are test inputs. The subject-rewrite code
also preserves a validated `dpu` or `vm` resource suffix, although this CAPD
lane exercises the `bm/cwe` workload identities above.

Like the real exchange, the mock also writes each component of the new subject
as a claim. For the controller identity above:

```json
{
  "dsx.nvidia.com/tenant": "nke",
  "dsx.nvidia.com/region": "capd",
  "dsx.nvidia.com/service": "nvke-controller",
  "dsx.nvidia.com/partition": "nvid.id",
  "dsx.nvidia.com/resource": [{"resource_type": "bm", "resource_id": "cwe"}]
}
```

## What the E2E lane proves

The `identity-exchange` lifecycle asserts that:

- CWE publishes the tenant kubeconfig and trust-bundle Secrets;
- the published exec credential selects an HTTPS `/v1/exchange` endpoint;
- each of the four management workloads obtains and exchanges a real local
  JWT-SVID;
- `kubectl auth whoami` reports the expected rewritten SPIFFE ID;
- the rewritten identity can list and delete nodes according to the rendered
  tenant RBAC; and
- a real node-list request succeeds, then the tenant-access Secrets disappear
  after cluster deletion.

The ordinary direct-SPIRE CAPD lane remains separate. That gives CI a control
path and prevents the exchange-specific topology from silently replacing the
existing authentication coverage.

## Run in CI

`.github/workflows/capd-dsx-identity-exchange-e2e.yml` invokes the shared
backend harness with:

- the `identity-exchange` Ginkgo label;
- the `dsx-identity-exchange` Tilt provider;
- post-init CNI installation, which avoids coupling this test to the kube-vip
  startup race; and
- an amd64 CAPD runner.

The workflow runs automatically for controller, agent, and Helm chart changes,
plus changes to its supporting workflow files. It does not run for nvkectl or
other CLI-only changes. It can also be started with `workflow_dispatch` after
the workflow exists on the default branch.

## Run locally

From `controller/`, run the existing local backend harness with the focused
provider and label:

```bash
LOCAL_NKE_E2E_PROVIDERS='["capi_core","bgp-containers","capd","capd-reboot-controller","dsx-identity-exchange"]' \
LOCAL_NKE_E2E_LABEL_FILTER=identity-exchange \
INSTALL_CNI_AFTER_INIT=true \
./hack/local-backend-e2e.sh
```

The harness checks its Docker, Kind, Tilt, Kubernetes, profile, and local CWE
configuration prerequisites before changing the environment. It preserves
CAPD containers from other runs unless
`LOCAL_NKE_E2E_CLEAN_STALE_NODES=true` is explicitly set. Set
`LOCAL_NKE_E2E_STATE_DIR` to keep logs and JUnit output in a specific directory.

The harness also installs the CAPNICo CRDs expected by the current controller.
It uses the committed CAPNICo CRD fixture by default; set
`CAPNICO_SOURCE_DIR` to read the CRDs from a provider checkout instead.

## Files

| File | Purpose |
|---|---|
| `main.go` | Starts the HTTPS server and connects its validator to the SPIRE Workload API. |
| `server.go` | Implements source validation, subject rewriting, ES256 signing, RFC 8693 exchange, discovery, and JWKS. |
| `deploy.yaml` | Deploys the mock, its TLS certificate, Workload API mount, Service, and SPIFFE registration. |
| `Tiltfile` | Wires the mock, CA distribution, controller subjects, and CWE Helm values into the CAPD environment. |
| `capi-ca-patch.yaml` | Mounts the test HTTPS CA into the CAPI, KCP, and CAPD managers. |
| `Dockerfile` | Builds the non-root test image from the repository's vendored dependencies. |

## Deliberate limitations

- The signing key is generated in memory and rotates whenever the pod restarts.
- Only the exchange, discovery, JWKS, readiness, and liveness endpoints needed
  by this lane are implemented; this is not a general OAuth or OIDC server.
- The mock validates SPIRE credentials and reproduces the subject rewrite used
  by the integration, but it does not reproduce production DSX policy,
  availability, federation, or network boundaries.
- CAPD uses container-backed nodes and a single control-plane node here. The
  lane does not qualify bare-metal IMDS node attestation or HA behavior.
