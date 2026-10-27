> **Generated file -- do not edit.** This page is written from the nvkectl
> command tree in `cli/internal/nvkectl`. To change what it says, change the
> command's own metadata -- its Short, Long, Example, Args, Aliases, deprecation
> notice or flag usage strings -- and run `make -C cli docs`. An edit made here
> is lost on the next run, and `make -C cli docs-check` fails until the
> committed pages match the tree again.

## nvkectl get providers

List infrastructure providers installed on the management cluster

### Aliases

`provider`

### Synopsis

List infrastructure providers installed on the management cluster.

This reads the site's Kubernetes cluster directly rather than the controller API, so it needs the kubeconfig registered with 'nvkectl config set-site --kubeconfig' and kubectl on PATH.

CONFIGURED means the provider's CRDs are established, its manager is available, and its credentials Secret exists. It does not mean those credentials work.

CREDENTIALS is the provider's own credential check, read from the NicoIdentity that names the provider's credentials Secret. nvkectl never runs a check: the provider checks every few minutes, and LAST CHECK is how long ago the latest check ran. A result is STALE when the provider's scheduled check is overdue or, if the provider publishes no usable schedule, when its last check, or its creation if it was never checked, is too old.

```
HEALTHY            the last check authenticated and read the current tenant
FAILING (reason)   the credentials are missing, invalid, rejected or forbidden
UNKNOWN (reason)   the check could not decide, for example during an outage
STALE              no check when one was due
PENDING            not checked yet
NO-IDENTITY        no NicoIdentity names the provider's credentials Secret
UNSUPPORTED        the provider does not serve NicoIdentity
UNREADABLE         nvkectl could not list NicoIdentities
-                  the provider has no credential health to report
```

HEALTHY does not prove that provisioning will succeed.

The set of providers listed is the set nvkectl knows how to describe. It is not a restriction: the controller accepts any infrastructure manifest written by hand.

Alpha: this command's output may change between releases.

```
nvkectl get providers [flags]
```

### Options

```
  -h, --help            help for providers
  -o, --output string   Output format: json or yaml (default: table)
```

### Options inherited from parent commands

```
      --json-logs     Emit logs as JSON
      --site string   Override the active site for this invocation
  -v, --verbose       Enable debug logging
```

### SEE ALSO

* [nvkectl get](nvkectl_get.md)	 - Display one or more resources

