# Integration Tests

The tests in this directory run against an existing deployment of NVKE. They exercise high level, end-to-end cluster lifecycle workflows in a black-box-like manner. The primary goal of these tests is to sanity check basic functionality as versions of dependent software rev. These tests are expensive in the sense that they use actual forge instances and take a long time to run (hours)

## Running the tests

Before you begin you need a test environment and usable forge instances. Currently the tests only interact with the CWE API and allow you to configure the test cluster however you want. This means you can point the tests at any deployment of NVKE (local, site-hosted, hybrid or other) and utilize forge resources from any site. The request files under `testdata/default` describe a cluster with a single cp node and a 1 node worker node pool, which the tests provision, scale and then tear down. Their site, VPC, subnet and instance-type IDs are placeholders (`00000000-0000-4000-8000-...`): real Forge identifiers are internal and are not kept in this repository (`hack/internal-config-check.sh` enforces this). A live run must supply its own request files:

- Copy the two files under `testdata/default` outside the repository and replace every Forge identifier with the target site's -- the placeholder site, VPC, subnet and instance-type IDs in both files, and `nico.orgID`, which carries a dev default rather than a placeholder
- Establish ssh tunnel to the site-controller
- Port-forward cloud-workflow-engine service
- `CREATE_CLUSTER_REQUEST_PATH=<copy> CREATE_NODE_POOL_REQUEST_PATH=<copy> go test -tags integration_test -v -timeout=3h` (from within this directory)

## Configuration

| Variable | Description | Default |
|----------|-------------|---------|
| `NKE_CONTROLLER_API_URL` | URL of the controller API. The old name `CWE_API_URL` keeps working until [nvke#1399](https://github.com/dsx-ai-factory/nvke/issues/1399) removes it, after the first controller and nvkectl GA releases that include the new names. | `http://localhost:8080/api` |
| `CREATE_CLUSTER_REQUEST_PATH` | Path to JSON file | `testdata/default/create-cluster-request.json` (placeholder IDs; override for a live run) |
| `CREATE_NODE_POOL_REQUEST_PATH` | Path to JSON file | `testdata/default/create-nodepool-request.json` (placeholder IDs; override for a live run) |
| `SKIP_CLEANUP` | Don't tear down any provisioned resources | `false` |

Example. The live suite sits behind the `integration_test` build tag, so the tag
is not optional: without it `go test` runs only `TestControllerAPIURL`, the
settings test that the ordinary unit lane runs, and reports success.

```bash
export NKE_CONTROLLER_API_URL=http://<site-controller>:8080/api
export CREATE_CLUSTER_REQUEST_PATH=~/nvke-integration/create-cluster-request.json
export CREATE_NODE_POOL_REQUEST_PATH=~/nvke-integration/create-nodepool-request.json
go test -tags integration_test -v -timeout=3h
```