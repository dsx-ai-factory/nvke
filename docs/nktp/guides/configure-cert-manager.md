# Configure cert-manager

NKTP depends on cert-manager to reap the benefits of following features. These features are not explicitly required, but are enabled by default and represent the best-practices for deploying NKTP.

## Installing NKTP without cert-manager

cert-manager has two uses in NKTP. Replace each to install without it:

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
