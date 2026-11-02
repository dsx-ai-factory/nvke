# Configure cert-manager

Choose how NVKE telemetry pipeline (NKTP) obtains its TLS certificates:

- **Use cert-manager (default and recommended).** cert-manager issues and renews
  certificates for internal Collector mTLS, the Operator's admission webhook,
  and Collector-to-Target-Allocator mTLS. These features are enabled by default
  and follow best practices for deploying NKTP.
- **Run without cert-manager.** Provide and renew your own certificates for
  internal Collector and Target Allocator mTLS. For the admission webhook, use
  your own certificates or let Helm generate them. Replace all three
  integrations before installing.

## Installing NKTP with cert-manager

Install cert-manager before installing NKTP with default values, following the
[cert-manager installation instructions](https://cert-manager.io/docs/installation/).
NKTP does not bundle or install cert-manager. The following sections describe
its three uses and the settings available when using it.

### Target Allocators

NKTP explicitly enables cert-manager-backed mTLS between each Collector
and its Target Allocator. The Operator provisions a CA plus server and client
certificates for each pair, as described in
[With cert-manager (default)](https://github.com/open-telemetry/opentelemetry-operator/blob/main/docs/target-allocator/README.md#with-cert-manager-default).

### Admission Webhook

NKTP uses an existing cert-manager installation to issue and renew the
Operator's admission webhook certificate by default. The upstream Operator
chart documents the available approaches under
[TLS Certificate Requirement](https://github.com/open-telemetry/opentelemetry-helm-charts/blob/opentelemetry-operator-0.126.0/charts/opentelemetry-operator/README.md#tls-certificate-requirement).

### Internal mTLS

By default, NKTP uses cert-manager to create a self-signed CA, issuer, and certificates for each collector 
used for collector-to-gateway and collector-to-watchdog traffic encryption.
`certManager.internalTLS.issuerRef` must reference a CA `Issuer` or `ClusterIssuer`
that populates `ca.crt` in the generated TLS Secrets, alongside `tls.crt` and
`tls.key`. NKTP mounts `ca.crt` to verify server and client certificates.
ACME issuers are unsupported for internal mTLS: they do not populate `ca.crt`,
so the required Secret volume mounts fail. See cert-manager's
[certificate Secret documentation](https://cert-manager.io/docs/usage/certificate/#target-secret).
Leave `issuerRef: {}` to use NKTP's generated internal CA and issuer.

```yaml
certManager:
  internalTLS:
    enabled: true
    issuerRef: {}
```

The cluster event reader, DaemonSet, and large-target metrics collectors each use a client certificate to send
telemetry to the gateways and watchdog over mTLS. Each gateway uses a server and client certificate.

Collector sidecars and the DaemonSet's Target Allocator and Operator log exporter
send to watchdog over OTLP/gRPC with mTLS on port 4317. Target Allocator exporters
are configured to send their own metrics over plaintext OTLP/gRPC on port 4320.

For a release in the `nktp` namespace:

```bash
kubectl get issuers,certs -n nktp
```

Example output:

```text
NAME                                                                  READY   AGE
issuer.cert-manager.io/nktp-internal-ca-issuer                        True    14s
issuer.cert-manager.io/nktp-internal-selfsigned-issuer                True    14s

NAME                                                                   READY   SECRET                                          AGE
certificate.cert-manager.io/nktp-cluster-events-client                 True    nktp-cluster-events-client-tls                  14s
certificate.cert-manager.io/nktp-daemonset-client                      True    nktp-daemonset-client-tls                       14s
certificate.cert-manager.io/nktp-gateway-logs-client                   True    nktp-gateway-logs-client-tls                    14s
certificate.cert-manager.io/nktp-gateway-logs-server                   True    nktp-gateway-logs-server-tls                    14s
certificate.cert-manager.io/nktp-gateway-metrics-client                True    nktp-gateway-metrics-client-tls                 14s
certificate.cert-manager.io/nktp-gateway-metrics-server                True    nktp-gateway-metrics-server-tls                 14s
certificate.cert-manager.io/nktp-gateway-traces-client                 True    nktp-gateway-traces-client-tls                  14s
certificate.cert-manager.io/nktp-gateway-traces-server                 True    nktp-gateway-traces-server-tls                  14s
certificate.cert-manager.io/nktp-internal-ca                           True    nktp-internal-ca                                14s
certificate.cert-manager.io/nktp-large-target-metrics-client           True    nktp-large-target-metrics-client-tls            14s
certificate.cert-manager.io/nktp-watchdog-server                       True    nktp-watchdog-server-tls                        14s
```

**NOTE:** Gateway-to-backend is **not** included as part of this feature. Users must configure this communication separately.

## Installing NKTP without cert-manager

Replace all three cert-manager integrations before installing. The following
values stop internal certificate creation and use Helm-generated admission
webhook certificates. Also provide internal Collector and Target Allocator
certificates as described below; these values alone are not sufficient.

```yaml
certManager:
  internalTLS:
    enabled: false
  targetAllocators:
    enabled: false
opentelemetry-kube-stack:
  opentelemetry-operator:
    admissionWebhooks:
      certManager:
        enabled: false
      autoGenerateCert:
        enabled: true
        recreate: false
```

### User-provided internal Collector certificates

Set `certManager.internalTLS.enabled: false` and create the TLS Secrets
referenced by the Collectors' and sidecars' `kubernetes.volumes` values in the
release namespace. Supply `ca.crt`, `tls.crt`, and `tls.key` with certificates
valid for the configured services and peers. Keeping the default Secret names,
mounts, and Collector TLS settings preserves internal mTLS. If you change names
or paths, update the volume references, mounts, and Collector configurations
together. You are responsible for issuing and renewing these certificates.

### User-provided Target Allocator certificates

Set `certManager.targetAllocators.enabled: false`, which sets
`mtls.useCertManager: false` on the watchdog, DaemonSet, and large-target
allocators, and reference certificates that already exist in the release
namespace. `mtls.enabled` stays a per-allocator setting. Apply the same `tls`
block under each enabled allocator at `watchdog.kubernetes.targetAllocator`,
`daemonset.kubernetes.targetAllocator`, and
`large_target_metrics.kubernetes.targetAllocator`. For example:

```yaml
daemonset:
  kubernetes:
    targetAllocator:
      mtls:
        enabled: true
        tls:
          certificateAuthorityCertificate:
            configMap:
              name: target-allocator-ca
          serverCertificate:
            certificateSecret:
              name: target-allocator-server
            keySecret:
              name: target-allocator-server
          clientCertificate:
            certificateSecret:
              name: target-allocator-client
            keySecret:
              name: target-allocator-client
```

See the Operator's
[user-provided certificate documentation](https://github.com/open-telemetry/opentelemetry-operator/blob/main/docs/target-allocator/README.md#without-cert-manager-user-provided-certificates)
for the accepted Secret and ConfigMap references, default keys, and certificate
rotation behavior.

### Webhook certificates without cert-manager

The values above let Helm generate a self-signed webhook certificate. This
configuration reuses that certificate on upgrades; it expires after 365 days by
default and is not renewed automatically.

To provide your own webhook certificate instead, also set
`admissionWebhooks.autoGenerateCert.enabled: false` and supply the upstream
chart's `admissionWebhooks.certFile`, `keyFile`, and `caFile` values under
`opentelemetry-kube-stack.opentelemetry-operator`. See the upstream chart's
[TLS Certificate Requirement](https://github.com/open-telemetry/opentelemetry-helm-charts/blob/opentelemetry-operator-0.126.0/charts/opentelemetry-operator/README.md#tls-certificate-requirement).

### Switching internal traffic to plaintext

Set `certManager.internalTLS.enabled: false` only with the matching configuration
and mount overrides in the [plaintext example](https://github.com/dsx-ai-factory/nvke/blob/main/docs/nktp/examples/disable-internal-mtls.values.yaml).
This flag stops certificate creation; it does not change Collector configurations.
The example changes internal traffic only; apply the webhook and Target Allocator
overrides above as well to remove the cert-manager dependency.
