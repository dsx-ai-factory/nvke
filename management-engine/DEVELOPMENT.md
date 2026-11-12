# Cluster Service Development

Run commands from `management-engine/` with GNU Make (`gmake` on macOS).
Generation targets install pinned tools. Change the sources, not generated files.

## Internal Kubernetes API

Edit Go types and kubebuilder markers under `apis/v1`. For a new kind, register
it with `SchemeBuilder` and update `PROJECT`. Add the new CRD to
`config/crd/kustomization.yaml`, which the Kustomize base consumes and
`make install-state` applies to the state cluster.

```sh
make generate manifests
make test-api
```

This regenerates deepcopy methods, canonical CRDs and RBAC, and
`dist/install.yaml`. Runtime manifests and permission rules are maintained under
`config/`. Preserve `PROJECT` metadata and scaffold markers.

## Helm chart generation

Run `make chart` after changing a runtime manifest or state definition. It runs
the pinned [Kubebuilder Helm plugin](https://book.kubebuilder.io/plugins/available/helm-v2-alpha)
in fresh staging, then applies the packaging extensions in `hack/chart/`.
`chart/` contains generated output; edit the canonical manifests or extension
sources rather than repairing Helm templates by hand.

The plugin supplies workload settings, probes, security contexts and runtime
values. Extensions supply state/controller switches, validated connection
references, conditional mounts and binding identities. Their insertion points
are checked against the pinned plugin output. Changes to ordinary workload
settings therefore propagate from Kustomize through generation.

`make chart-check chart-assets-check` verifies the generated chart and all
installation modes. Fresh staging regenerates runtime defaults before adding
chart-specific values; in-place plugin generation would preserve old values.

Review and commit generated changes together with their sources.
