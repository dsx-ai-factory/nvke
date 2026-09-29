# Integration Tests

The tests in this directory run against an existing deployment of NKE. They exercise high level, end-to-end cluster lifecycle workflows in a black-box-like manner. The primary goal of these tests is to sanity check basic functionality as versions of dependent software rev. These tests are expensive in the sense that they use actual forge instances and take a long time to run (hours)

## Running the tests

Before you begin you need a test environment and usable forge instances. Currently the tests only interact with the CWE API and allow you to configure the test cluster however you want. This means you can point the tests at any deployment of NKE (local, site-hosted, hybrid or other) and utilize forge resources from any site. The request files under `testdata/default` describe a cluster with a single cp node and a 1 node worker node pool, which the tests provision, scale and then tear down. Their site, VPC, subnet and instance-type IDs are placeholders (`00000000-0000-4000-8000-...`): real Forge identifiers are internal and are not kept in this repository (`hack/internal-config-check.sh` enforces this). A live run must supply its own request files:

- Copy the two files under `testdata/default` outside the repository and replace the placeholder IDs with the target site's
- Establish ssh tunnel to the site-controller
- Port-forward cloud-workflow-engine service
- `CREATE_CLUSTER_REQUEST_PATH=<copy> CREATE_NODE_POOL_REQUEST_PATH=<copy> go test -v -timeout=3h` (from within this directory)

## Configuration

| Variable | Description | Default |
|----------|-------------|---------|
| `CWE_API_URL` | URL of CWE API | `localhost:8080/api` |
| `CREATE_CLUSTER_REQUEST_PATH` | Path to JSON file | `testdata/default/create-cluster-request.json` (placeholder IDs; override for a live run) |
| `CREATE_NODE_POOL_REQUEST_PATH` | Path to JSON file | `testdata/default/create-nodepool-request.json` (placeholder IDs; override for a live run) |
| `SKIP_CLEANUP` | Don't tear down any provisioned resources | `false` |

Example:
```bash
export CWE_API_URL=http://az24-nke-sc:8080/api
go test -v -timeout=3h
```