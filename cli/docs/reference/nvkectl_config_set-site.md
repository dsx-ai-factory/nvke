> **Generated file -- do not edit.** This page is written from the nvkectl
> command tree in `cli/internal/nvkectl`. To change what it says, change the
> command's own metadata -- its Short, Long, Example, Args, Aliases, deprecation
> notice or flag usage strings -- and run `make -C cli docs`. An edit made here
> is lost on the next run, and `make -C cli docs-check` fails until the
> committed pages match the tree again.

## nvkectl config set-site

Configure the management plane the CLI connects to

`--proxy-client-cert`, `--proxy-client-key` must be used together.

### Synopsis

Store the URL and management cluster kubeconfig for a management plane in ~/.config/nvkectl/config.yaml.

The entry for --name is replaced by the values given in this call, and the site
becomes the current site. Run 'nvkectl config use-site &lt;name&gt;' to switch between
configured sites.

For sites fronted by a proxy that requires TLS client authentication, pass
--proxy-client-cert and --proxy-client-key. nvkectl presents the certificate to
the proxy on every request.

--otlp-endpoint is optional. It is the OTLP/gRPC endpoint (host:port or an
http(s):// URL; http implies --otlp-insecure) that receives the spans and log
records of commands that use this site. Headers for the endpoint come from
OTEL_EXPORTER_OTLP_HEADERS and are never stored.

```
nvkectl config set-site [flags]
```

### Options

```
  -h, --help                       help for set-site
      --kubeconfig string          Path to the management cluster kubeconfig (required)
      --name string                Name for this site entry. Must not be empty (default "default")
      --otlp-endpoint string       OTLP/gRPC endpoint (host:port or an http(s):// URL; http implies --otlp-insecure) that receives the spans and log records of commands that use this site (optional)
      --otlp-insecure              Send to --otlp-endpoint without TLS. An http:// endpoint is always insecure
      --proxy-client-cert string   Path to the proxy TLS client certificate
      --proxy-client-key string    Path to the private key matching --proxy-client-cert
      --url string                 Controller API base URL, e.g. http://localhost:8080 (required)
```

### Options inherited from parent commands

```
      --json-logs     Emit logs as JSON
      --site string   Override the active site for this invocation
  -v, --verbose       Enable debug logging
```

### SEE ALSO

* [nvkectl config](nvkectl_config.md)	 - Manage nvkectl configuration

