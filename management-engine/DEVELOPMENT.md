# Management Engine Development

Run commands from `management-engine/` with GNU Make (`gmake` on macOS).
Generation targets install pinned tools. Change the sources, not generated files.

## Internal Kubernetes API

Edit Go types and kubebuilder markers under `apis/v1`. For a new kind, register
it with `SchemeBuilder` and update `PROJECT`. Add Management Engine-specific
CRDs to `config/crd/kustomization.yaml`. Catalog CRDs belong in the shared chart
under `../controller/deploy/charts/nvke-crds/bases` and its `kustomization.yaml`;
adding a catalog kind also requires updating the generation routing in `Makefile`.

```sh
make generate manifests
make test-api
```

This regenerates deepcopy methods, CRDs, RBAC, `dist/install.yaml`, and the Helm
chart. Preserve `PROJECT` metadata and scaffold markers; do not use `--force`
to overwrite maintained chart settings.

Generation writes MachineImage, ClusterRelease and NodePoolRelease to their
canonical files in the shared catalog chart. Verify those files and their Helm
rendering after changing the catalog types:

```sh
make -C .. chart-crds-check
make test-chart-crds
```

`make -C .. chart-crds` optionally renders the catalog into ignored
`dist/release-crds.yaml` for tools such as Tilt; it does not update tracked schemas.

Review and commit generated changes together with their sources.
