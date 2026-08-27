# nico-mock

A local, Docker-backed stand-in for the parts of the NICo and Forge APIs that the
Cloud Workflow Engine uses during development.

It exists so a developer can drive the cluster-create flow without real bare
metal and without NVIDIA network access. It provisions kind-style node
containers, serves the discovery and instance routes the local NICo and legacy
Forge flows need, and reconstructs API responses from Docker labels and
`docker inspect`.

It is a development and test tool. Nothing here ships to a site.

## Building and running

```bash
make -C controller/hack/nico-mock build
make -C controller/hack/nico-mock test
make -C controller/hack/nico-mock lint
```

The binary is `nico-mock`. Root `make build` links it along with the other
binaries, so you do not need to build it from this directory.

It needs a working Docker daemon, because the containers it creates and inspects
are real.

## Where it is used

The controller's local development and test flows point at it instead of the real
provider. CI lints and unit-tests it as its own module, in `lint.yml` and
`controller-test.yml`.
