# Secure gateway export

NKTP ships with a self-hosted LGTM stack sending telemetry over plaintext by default. This is intended only for local development. Production environments should configure HTTPS before sending logs, metrics and traces to a production backend.

This guide walks through setting up mTLS for NKTP's three gateway collectors to an OTLP/HTTP backend. This guide does not focus on encryption from other components to the gateways. The [example](../examples/gateway-mtls.values.yaml) shows the skeleton for using mTLS.

## mTLS with an existing certificate

As a prerequisite, you need to already have:
- the CA certificate used to verify the backend server certificate;
- a client certificate signed by a CA that the backend trusts; and
- the private key corresponding to the client certificate.

1. Create a secret containing all three files in the Helm release namespace.

```bash
kubectl create secret generic backend-tls \
  --namespace <release-namespace> \
  --from-file=ca.crt=/path/to/backend-server-ca.crt \
  --from-file=tls.crt=/path/to/client.crt \
  --from-file=tls.key=/path/to/client.key
```

2. Mount the secret into each gateway.

```yaml
gateway_logs:
  kubernetes:
    podSecurityContext:
      fsGroup: 10001
    volumes:
      - name: backend-tls
        projected:
          defaultMode: 0440
          sources:
            - secret:
                name: backend-tls
                items:
                  - key: ca.crt
                    path: ca.crt
                  - key: tls.crt
                    path: tls.crt
                  - key: tls.key
                    path: tls.key
    volumeMounts:
      - name: backend-tls
        mountPath: /etc/nktp/backend-tls
        readOnly: true
```

> [!NOTE]
> `fsGroup: 10001` matches the group used by NKTP's default Collector image. It
> lets the non-root Collector read the root-owned Secret files through the group
> read bit in `0440`. If you use a custom Collector image, set `fsGroup` to a
> group available to that image. Alternatively, omit both `defaultMode` and
> `fsGroup` to use Kubernetes' default Secret mode (`0644`).

3. Configure the exporter to use the mounted certificate.

```yaml
gateway_logs:
  ...
  otelcol:
    config:
      exporters:
        otlp_http:
          endpoint: https://telemetry.example.com:4318
          tls:
            ca_file: /etc/nktp/backend-tls/ca.crt
            cert_file: /etc/nktp/backend-tls/tls.crt
            key_file: /etc/nktp/backend-tls/tls.key
            reload_interval: 1m
```

Apply the volume, mount, and exporter settings to `gateway_logs`,
`gateway_metrics`, and `gateway_traces`. The complete
[values example](../examples/gateway-mtls.values.yaml) configures all three.

For server-authenticated TLS with a private CA but no client authentication,
mount only `ca.crt` and configure only `ca_file`.

For failures, inspect gateway logs for `x509`, `tls`, `handshake`,
`unauthorized`, or `forbidden`. Common causes are a missing intermediate CA, an
endpoint hostname absent from the server certificate, an expired client
certificate, or a client issuer the backend does not trust.

## Automate client certificate with cert-manager 

[cert-manager](https://cert-manager.io/docs/configuration/) does not ship with
NKTP. It can issue and renew the gateway client certificate when an existing
`Issuer` or `ClusterIssuer` represents a CA that the backend trusts. Creating an
issuer does not by itself make the backend trust that CA.

1. Ensure a `ClusterIssuer` or `Issuer` CR exists for a Certificate Authority that your backend trusts. For details, go to the [cert-manager](https://cert-manager.io/docs/configuration/) documentation.

2. Create a `Certificate` CR in the `nktp` namespace.

```yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: nktp-gateway-client
  namespace: {{ namespace }}
spec:
  secretName: backend-tls
  issuerRef:
    kind: ClusterIssuer
    name: backend-client-ca
  usages:
    - client auth
  commonName: example.com
  privateKey:
    rotationPolicy: Always
```

cert-manager maintains `tls.crt` and `tls.key` in `backend-tls`. Mount
that Secret in every gateway and retain `cert_file`, `key_file`, and
`reload_interval` in the exporter configuration.
