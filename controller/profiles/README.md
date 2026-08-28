# Supply OS and config profiles to the controller

## Start with the example

[`example/`](example/) is a self-contained Ubuntu 24.04 amd64 profile bundle. It
contains an OS profile, a worker config profile, a runbook, and the Dockerfile
needed to package them.

The controller does not load profiles from this directory when deployed. Copy
the example to a separate repository or another location outside
`nke/controller`, then replace every `REPLACE_*` value for your environment:

```console
cp -R controller/profiles/example ../controller-profiles
cd ../controller-profiles
grep -R 'REPLACE_' .
```

Set `REPLACE_BUILD_DATE` to `YYYY-MM-DD`. Set
`REPLACE_KUBERNETES_VERSION` to the version supplied by
`REPLACE_APT_PACKAGES`. The boot asset base must contain `vmlinuz` and `initrd`
from the Ubuntu ISO named by `REPLACE_UBUNTU_24_04_SERVER_ISO_URL`. The apt
setup command and package list must install `nke-agent` and the Kubernetes
packages required by that version.

Confirm that no placeholders remain before building:

```console
test -z "$(grep -R 'REPLACE_' .)"
```

The copied bundle has this layout:

```text
controller-profiles/
├── Dockerfile
├── profiles/
│   ├── configProfiles/
│   └── osProfiles/
└── runbooks/
```

Customize or add profiles while preserving this layout.

## Build the content image

Build and push the content image to a registry accessible from the target
cluster:

```console
docker build -t ghcr.io/example/controller-profiles:v1 .
docker push ghcr.io/example/controller-profiles:v1
```

## Configure the controller

Configure the controller Helm chart to consume the image:

```yaml
podSecurityContext: null

profiles:
  path: /profiles
  image:
    enabled: true
    repository: ghcr.io/example/controller-profiles
    tag: v1

spiffe:
  enabled: false
```

The chart's `materialize-profiles` init container copies `/profiles` from the
content image into the directory the controller reads. If the registry is
private, also configure `imagePullSecrets` with credentials that can pull the
image. This example uses plaintext controller-to-agent transport for evaluation;
configure SPIFFE before using it in production. Setting `podSecurityContext` to
`null` also runs the controller and materializer as root; restore a non-root
materialization design for production.
