# Configure cert-manager

NKTP depends on cert-manager to reap the benefits of following features. These features are not explicitly required, but are enabled by default and represent the best-practices for deploying NKTP.

## Installing NKTP without cert-manager

cert-manager has three uses in NKTP. The following values disable internal
certificate creation and switch the admission webhook to Helm-generated
certificates. Before installing, also configure user-provided Target Allocator
certificates as described below and complete the internal plaintext migration
at the end of this guide.

```yaml
certManager:
  internalTLS:
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

## Usages

### Target Allocators

NKTP explicitly enables cert-manager-backed mTLS between each Collector
and its Target Allocator. The Operator provisions a CA plus server and client
certificates for each pair, as described in
[With cert-manager (default)](https://github.com/open-telemetry/opentelemetry-operator/blob/main/docs/target-allocator/README.md#with-cert-manager-default).

As an alternative, set `useCertManager: false` and reference certificates that
already exist in the release namespace. Apply the same `mtls` block under each
enabled allocator at `watchdog.kubernetes.targetAllocator`,
`daemonset.kubernetes.targetAllocator`, and
`large_target_metrics.kubernetes.targetAllocator`. For example:

```yaml
daemonset:
  kubernetes:
    targetAllocator:
      mtls:
        enabled: true
        useCertManager: false
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

### Admission Webhook

NKTP uses an existing cert-manager installation to issue and renew the
Operator's admission webhook certificate by default. The upstream Operator
chart documents the available approaches under
[TLS Certificate Requirement](https://github.com/open-telemetry/opentelemetry-helm-charts/blob/opentelemetry-operator-0.123.0/charts/opentelemetry-operator/README.md#tls-certificate-requirement).

As an alternative, disable cert-manager integration and let Helm generate a
self-signed certificate. This configuration reuses that certificate on
upgrades; it expires after 365 days by default and is not renewed automatically.

```yaml
opentelemetry-kube-stack:
  opentelemetry-operator:
    admissionWebhooks:
      certManager:
        enabled: false
      autoGenerateCert:
        enabled: true
        recreate: false
```

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

#### Switching internal traffic to plaintext

Set `certManager.internalTLS.enabled: false` only with the matching configuration
and mount changes below. This flag stops certificate creation; it does not change
Collector configurations. For a detailed example of the overrides required, look at the [example](../examples/disable-internal-mtls.values.yaml).
