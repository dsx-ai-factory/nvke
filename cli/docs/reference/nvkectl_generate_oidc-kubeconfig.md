> **Generated file -- do not edit.** This page is written from the nvkectl
> command tree in `cli/internal/nvkectl`. To change what it says, change the
> command's own metadata -- its Short, Long, Example, Args, Aliases, deprecation
> notice or flag usage strings -- and run `make -C cli docs`. An edit made here
> is lost on the next run, and `make -C cli docs-check` fails until the
> committed pages match the tree again.

## nvkectl generate oidc-kubeconfig

Generate an oidc-login kubeconfig from an input kubeconfig

### Required options

`--kubeconfig`, `--oidc-client-id`, `--oidc-issuer-url`

### Synopsis

Write an oidc-login kubeconfig from an input kubeconfig.

The cluster server and certificate authority come from the input file's current
context. The input user is not copied. The generated user is an exec plugin:

```
kubectl oidc-login get-token --oidc-issuer-url ... --oidc-client-id ...
```

Nothing is sent to the cluster. After OIDC is enabled on the API server, the
file can be passed to config set-site --kubeconfig.

By default the kubeconfig is printed to stdout. Use --output-file to write it
to a file instead.

--headless stamps --grant-type=device-code and --skip-open-browser. kubectl
prints a URL to open on another machine. The identity provider client must
allow the device authorization grant.

```
nvkectl generate oidc-kubeconfig [flags]
```

### Examples

```
  nvkectl generate oidc-kubeconfig \
    --kubeconfig ./cluster.kubeconfig \
    --oidc-issuer-url https://idp.example/realms/nke \
    --oidc-client-id nvke-cli \
    --headless \
    --output-file ./oidc.kubeconfig

  nvkectl config set-site --name my-tier0-site \
    --url https://controller.my-tier0-site.example.com \
    --kubeconfig ./oidc.kubeconfig
```

### Options

```
      --headless                       Use the device authorization grant so login works without a local browser
  -h, --help                           help for oidc-kubeconfig
      --kubeconfig string              Path to the input kubeconfig (required)
      --oidc-client-id string          OIDC client ID (required)
      --oidc-extra-scope stringArray   Extra OIDC scope. Repeatable.
      --oidc-issuer-url string         OIDC issuer URL (required)
      --output-file string             Write kubeconfig to file instead of stdout
```

### Options inherited from parent commands

```
      --json-logs     Emit logs as JSON
      --site string   Override the active site for this invocation
  -v, --verbose       Enable debug logging
```

### SEE ALSO

* [nvkectl generate](nvkectl_generate.md)	 - Generate NVKE configuration files

