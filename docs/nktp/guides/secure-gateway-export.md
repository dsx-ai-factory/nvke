# Secure gateway export

NVKE telemetry pipeline (NKTP) exports to its bundled LGTM backend over plaintext OTLP/gRPC at
`lgtm:4317` by default. LGTM already enables both
[gRPC and HTTP ingestion](https://github.com/grafana/docker-otel-lgtm/blob/v0.38.0/docker/otelcol-config.yaml).
This backend is intended only for local development. Configure TLS before
sending logs, metrics and traces to a production backend.

Choose the protocol and endpoint supported by your backend for each signal.
The Collector's [`otlp` exporter](https://github.com/open-telemetry/opentelemetry-collector/blob/v0.162.0/exporter/otlpexporter/README.md)
uses gRPC; [`otlp_http`](https://github.com/open-telemetry/opentelemetry-collector/blob/v0.162.0/exporter/otlphttpexporter/README.md)
uses HTTP. Changing the port alone does not change the protocol.
For example, direct ingestion into [Prometheus](https://prometheus.io/docs/guides/opentelemetry/)
or [Loki](https://grafana.com/docs/loki/latest/send-data/otel/) requires OTLP/HTTP.
LGTM accepts gRPC through its bundled Collector, which forwards metrics and logs
to those HTTP endpoints internally.

This guide configures mTLS for the three gateway Collectors using gRPC. The
[example](https://github.com/dsx-ai-factory/nvke/blob/main/docs/nktp/examples/gateway-mtls.values.yaml) shows the overrides. Receiver
TLS for connections into the gateways is configured separately.

## mTLS with an existing certificate

As a prerequisite, you need to already have:
- the CA certificate used to verify the backend server certificate;
- a client certificate signed by a CA that the backend trusts; and
- the private key corresponding to the client certificate.

1. Create a secret containing all three files in the Helm release namespace.
   Edit the namespace and local certificate paths below before running:

```bash
NKTP_NAMESPACE=nktp
BACKEND_CA_FILE=/path/to/backend-server-ca.crt
BACKEND_CLIENT_CERT_FILE=/path/to/client.crt
BACKEND_CLIENT_KEY_FILE=/path/to/client.key
kubectl create secret generic backend-tls \
  --namespace "$NKTP_NAMESPACE" \
  --from-file="ca.crt=$BACKEND_CA_FILE" \
  --from-file="tls.crt=$BACKEND_CLIENT_CERT_FILE" \
  --from-file="tls.key=$BACKEND_CLIENT_KEY_FILE"
```

2. Mount the secret into each gateway, preserving its internal TLS volumes and
mounts. Each gateway needs its internal server certificate for ingress, and its sidecar needs its internal
client certificate to export to watchdog. The backend credentials belong on the main gateway Collector, not its sidecar.

```yaml
gateway_logs:
  kubernetes:
    podSecurityContext:
      fsGroup: 10001
    volumes:
      - name: internal-tls-server
        secret:
          secretName: nktp-gateway-logs-server-tls
          items:
            - {key: tls.crt, path: tls.crt}
            - {key: tls.key, path: tls.key}
            - {key: ca.crt, path: ca.crt}
      - name: internal-tls-client
        secret:
          secretName: nktp-gateway-logs-client-tls
          items:
            - {key: tls.crt, path: tls.crt}
            - {key: tls.key, path: tls.key}
            - {key: ca.crt, path: ca.crt}
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
      - name: internal-tls-server
        mountPath: /etc/nktp/internal-tls/server
        readOnly: true
      - name: backend-tls
        mountPath: /etc/nktp/backend-tls
        readOnly: true
```

> **Note:** `fsGroup: 10001` matches the group used by NKTP's default Collector image. It
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
        otlp:
          endpoint: telemetry.example.com:4317
          tls:
            insecure: false
            ca_file: /etc/nktp/backend-tls/ca.crt
            cert_file: /etc/nktp/backend-tls/tls.crt
            key_file: /etc/nktp/backend-tls/tls.key
            reload_interval: 1m
```

Apply the volume, mount, and exporter settings to `gateway_logs`,
`gateway_metrics`, and `gateway_traces`. Use each gateway's own internal Secrets:
`nktp-gateway-<signal>-server-tls` and `nktp-gateway-<signal>-client-tls`, where
`<signal>` is `logs`, `metrics`, or `traces`. Keep the volume names and mount paths
shown above. The complete [values example](https://github.com/dsx-ai-factory/nvke/blob/main/docs/nktp/examples/gateway-mtls.values.yaml)
configures all three.

Set `insecure: false` explicitly: the development defaults disable TLS, and
Helm retains nested defaults when merging overrides. Configure the watchdog
and its sidecar's backend exporters too, if sending NKTP's own telemetry to
this backend.

For an HTTP backend, remove the default gRPC exporter with `otlp: null`, add
`otlp_http` with an `https://` endpoint and the same certificate settings, and
replace `otlp` with `otlp_http` in each affected pipeline's `exporters` list.
Preserve the queue and retry settings from the default exporter as needed.

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

2. Create a `Certificate` CR in the `nktp` namespace. Change `metadata.namespace`
   to match your Helm release namespace.

```yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: nktp-gateway-client
  namespace: nktp
spec:
  secretName: backend-tls
  issuerRef:
    kind: ClusterIssuer
    name: backend-client-ca
  usages:
    - client auth
  commonName: example.com # Placeholder: the client identity your backend expects.
  privateKey:
    rotationPolicy: Always
```

Replace `commonName` with the identity your backend maps to the client, or set
the `dnsNames` or `uris` it reads instead.

cert-manager maintains `tls.crt` and `tls.key` in `backend-tls`. It writes
`ca.crt` only when the issuer supplies one, and that is the CA of the client
certificate, not of the backend's server certificate.

3. Create a separate Secret with the CA that signs the backend's server
   certificate. Edit the namespace and local certificate path below before
   running:

```bash
NKTP_NAMESPACE=nktp
BACKEND_CA_FILE=/path/to/backend-server-ca.crt
kubectl create secret generic backend-ca \
  --namespace "$NKTP_NAMESPACE" \
  --from-file="ca.crt=$BACKEND_CA_FILE"
```

4. In every gateway, replace the `backend-tls` volume from the previous section
   with one that projects both Secrets. Keep the other volumes and the mounts;
   the mount path and file names stay the same:

```yaml
      - name: backend-tls
        projected:
          defaultMode: 0440
          sources:
            - secret:
                name: backend-tls
                items:
                  - key: tls.crt
                    path: tls.crt
                  - key: tls.key
                    path: tls.key
            - secret:
                name: backend-ca
                items:
                  - key: ca.crt
                    path: ca.crt
```

Retain `ca_file`, `cert_file`, `key_file`, and `reload_interval` in the
exporter configuration.
