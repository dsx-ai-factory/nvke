# Management Engine Development

Run commands from `management-engine/` with GNU Make (`gmake` on macOS).
Generation targets install pinned tools. Change the sources, not generated files.

## Internal Kubernetes API

Edit Go types and kubebuilder markers under `apis/v1`. For a new kind, register
it with `SchemeBuilder`, update `PROJECT`, and add its CRD to
`config/crd/kustomization.yaml`.

```sh
make generate manifests
make test-api
```

This regenerates deepcopy methods, CRDs, RBAC, `dist/install.yaml`, and the Helm
chart. Preserve `PROJECT` metadata and scaffold markers; do not use `--force`
to overwrite maintained chart settings.

After changing MachineImage, ClusterRelease, or NodePoolRelease, also refresh
the NVKE controller chart's embedded CRDs:

```sh
make -C .. chart-crds
make -C .. chart-crds-check
```

## Public tenant API

Maintain the proto and exposure configuration under `public/tenant/apis/`.
Regenerate the internal CRDs first when changing internal Go types.

```sh
make generate-public-api
make check-public-api
make test-public-contract test-unit
```

Generation produces protobuf, gRPC and HTTP gateway bindings, OpenAPI, field
references, and private spec mappings. See the [tenant API contract](public/tenant/README.md)
for exposure rules and intentional compatibility-lock updates.

Review and commit generated changes together with their sources.
