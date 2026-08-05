# Integration Tests

The tests in this directory run against an existing deployment of NKE. They exercise high level, end-to-end cluster lifecycle workflows in a black-box-like manner. The primary goal of these tests is to sanity check basic functionality as versions of dependent software rev. These tests are expensive in the sense that they use actual forge instances and take a long time to run (hours)

## Running the tests

Before you begin you need a test environment and usable forge instances. Currently the tests only interact with the CWE API and allow you to configure the test cluster however you want. This means you can point the tests at any deployment of NKE (local, site-hosted, hybrid or other) and utilize forge resources from any site. That being said, the primary use case will be regularly scheduled runs against az24 in Forge-Tenant-NKE which is why we have defaults set accordingly.

Without any overrides the tests will provision a cluster with a single cp node, scale in a 1 node worker node pool and then tear it all down. To run the test with these configured defaults against the site-controller in az24/Forge-Tenant-NKE you can simply:

- Establish ssh tunnel to the site-controller
- Port-forward cloud-workflow-engine service
- `go test -v -timeout=3h` (from within this directory)

## Configuration

| Variable | Description | Default |
|----------|-------------|---------|
| `CWE_API_URL` | URL of CWE API | `localhost:8080/api` |
| `CREATE_CLUSTER_REQUEST_PATH` | Path to JSON file | `testdata/default/create-cluster-request.json` |
| `CREATE_NODE_POOL_REQUEST_PATH` | Path to JSON file | `testdata/default/create-nodepool-request.json` |
| `SKIP_CLEANUP` | Don't tear down any provisioned resources | `false` |

Example:
```bash
export CWE_API_URL=http://az24-nke-sc:8080/api
go test -v -timeout=3h
```