# osprofiles-to-crs

Converts an OS profile tree into `MachineImage`, `ClusterRelease` and
`NodePoolRelease` custom resources (CRDs).

## Status: temporary

We are moving from the `osProfiles` files to these CRDs, and the move is in
progress. This tool exists only to produce the CRDs from the files while both
exist. It is temporary: once the move is complete it is deprecated and removed
together with the `osProfiles` files. Do not build anything long-lived on it.

## What it does

For each profile in `osProfiles/metadata.yaml` (and `deprecated.yaml`, if
present) it reads the profile's `ipxeScript` and `userData.tmpl` and writes three
manifests into a flat output directory, all named after the profile:

- `<profile>-machineimage.yaml`
- `<profile>-nodepoolrelease.yaml`
- `<profile>-clusterrelease.yaml`

An SPDX header after the first line of either file is not copied into the manifests.

Because the names are the profile names, the `osProfile` values that clusters
and node pools already store keep resolving.

- **Built from the CRD types.** The manifests are constructed from the Go types
  in `management-engine/apis/v1`, so field names and shapes come from the CRDs
  themselves.
- **Validated before anything is written.** Every manifest is admitted against
  the CRDs (structural defaulting, the OpenAPI schema and the CEL rules), and the
  profile name must be a valid Kubernetes object name. A profile the API server
  would reject fails here.
- **Append-only for published content.** A `MachineImage` spec and the release
  specs are immutable once applied, so regenerating into an existing output
  directory may only change `spec.lifecycle.deprecated` on a manifest that is
  already there. If a published spec would change, it fails; publish a new
  profile name instead.
- **Deprecated profiles keep their names** so existing pools still resolve them.
  `deprecated.yaml` only sets `spec.lifecycle.deprecated`, which excludes the
  release from upgrade-target selection.
- The Kubernetes version is carried as an annotation on the releases, and the
  variant is copied as written.

## Usage

Run it from `controller/`:

```console
go run ./hack/osprofiles-to-crs --profiles <profiles root> --output <directory>
```

`--profiles` is a directory that contains `osProfiles/`, the same root the
controller reads as `PROFILES_DIR`.

| Flag | Default | Meaning |
|---|---|---|
| `--profiles` | required | Profiles root containing `osProfiles/` |
| `--output` | required | Flat directory the manifests are written to |
| `--crds` | `../management-engine/apis/crds` | CRD manifests every manifest is validated against |
| `--license` | `LicenseRef-NvidiaProprietary` | SPDX identifier in the generated file headers; `Apache-2.0` for files that live in this repository |
| `--gitops` | `true` | Add the Argo CD sync-wave and `Prune=false` annotations; use `--gitops=false` for manifests applied another way (kubectl, Tilt, envtest) |
| `--check` | `false` | Write nothing; fail when the output directory is not an exact regeneration |

The in-repo fixtures are generated with:

```console
make fixture-releases
```

which runs the tool on `test/fixtures/profiles` into `test/fixtures/releases`
with `--license Apache-2.0 --gitops=false`. `go test ./hack/osprofiles-to-crs`
runs the same command with `--check`, so the committed fixtures cannot drift from
the profiles.

To confirm a directory is up to date without changing it:

```console
go run ./hack/osprofiles-to-crs --profiles <profiles root> --output <directory> --check
```
