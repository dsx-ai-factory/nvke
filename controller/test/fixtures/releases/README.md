# Fixture MachineImage and release manifests

The controller reads OS images and releases from `MachineImage`,
`ClusterRelease` and `NodePoolRelease` custom resources. These manifests are
this repository's own set for local runs and tests: one `MachineImage`, one
`ClusterRelease` and one `NodePoolRelease` per release.

Maintained by hand. `go test ./internal/clients/nico` renders the bootstrap
user-data out of the `*-nodepoolrelease.yaml` files here, so a malformed one
fails there.

Tilt applies them by default (`NVKE_CONTROLLER_RELEASES_DIR` overrides the directory), the
gofail e2e installs them into envtest, and `make releases-install` applies them
to the cluster in `KUBECONFIG`.
