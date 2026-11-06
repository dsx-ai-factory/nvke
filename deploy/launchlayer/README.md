# deploy/launchlayer

The deploy path for an NVKE management cluster whose tenant clusters run on LaunchLayer, through CAPLL. It is [`deploy/nico`](../nico/) with CAPNICo swapped for CAPLL, run by the same [`nvke-deploy.sh`](../nvke-deploy.sh). Read [`deploy/README.md`](../README.md) for the inputs and the commands; this page covers what differs.

## What differs from deploy/nico

| | deploy/nico | deploy/launchlayer |
|---|---|---|
| Infrastructure provider | `capnico`, a manifest item | `capll`, a helm item installing CAPLL's chart after `spire`, with the manager's flags from the stack config and the SPIFFE exec-auth plugin. `nvke-base-config` grants it Secret `get` in `nvke-system`, where NVKE writes the bootstrap Secrets CAPLL reads |
| Provider credentials | `nico-credentials` in `capnico-system`, from `nico.*` and `NVKECTL_NICO_TOKEN` | `launchlayer-credentials` in `capll-system`, from `launchLayer.*` and `NVKECTL_LAUNCHLAYER_CLIENT_SECRET` |
| `supportedInfraProviders` | `nico,infra` | `launchLayer` |
| Node attestation | `tpm` | `dsxImds` |
| SPIRE `nvke-service` identity | `nvke-controller` and the CAPI namespaces | also `capll-system` |

## The stack config

Start from [`examples/stack-config-launchlayer.json`](../examples/stack-config-launchlayer.json). It has a `launchLayer` object in place of `nico`:

| Key | Required | Value |
|---|---|---|
| `launchLayer.baseURL` | yes | The address every LaunchLayer service resolves against: CAPLL's `--base-url` |
| `launchLayer.vpcURL`, `computeURL`, `sgURL` | no | Override `baseURL` for one service. Empty inherits it |
| `launchLayer.issuerURL` | yes | The OAuth2 issuer CAPLL gets its token from |
| `launchLayer.clientID` | yes | The OAuth2 client. Its secret comes from `NVKECTL_LAUNCHLAYER_CLIENT_SECRET`, not the file |
| `launchLayer.scopes` | no | Space-separated scopes the token must carry, as the LaunchLayer gateway requires |

`NVKECTL_NICO_TOKEN` is not read. The node login and database password variables are, as for `deploy/nico`.

Without `--release`, set `images.profiles`: the controller enables a profiles image and nothing else names it, so the render stops without it. See [Images](#images) for the controller image.

## What the site supplies

The path installs CAPLL and configures the controller for LaunchLayer. Two settings depend on the site's DSX identity service and are passed at install time with `--values-for`:

- **`nodeAgent.dsxImds.operatorTenant`** for the controller (`--values-for controller=FILE`). It must match DSX Identity's `global.operatorTenant`; the chart refuses to render without it. Set `nodeAgent.dsxImds.endpoint` too if the metadata service is not at the default `http://169.254.169.254`.
- **The `dsx_imds` node attestor** for the SPIRE server (`--values-for spire=FILE`), with `jwks_url`, `issuer` and `audience` naming DSX Identity. The nodes' image must ship the agent plugin. See [infrastructure-providers.md](../../docs/site-bootstrap/infrastructure-providers.md#launchlayer).

## Images

Like `deploy/nico`, the path renders no image pull secret, so the node has to be able to pull every image as named. The controller and profiles images come from the release, or from `images.controller` and `images.profiles`. CAPLL's chart runs the image released with it, `ghcr.io/dsx-ai-factory/cluster-api-provider-launchlayer/controller:v0.2.0`: no release artifact binds either yet, and the stack config's `mirror` does not apply to them. While CAPLL's chart and image are private, run `helm registry login ghcr.io` before installing, and load the image into the node's containerd or configure containerd with a credential or mirror for `ghcr.io`. The overlay's default controller image is private too.

## Install

```bash
deploy/nvke-deploy.sh install --deploy-path deploy/launchlayer \
  --sitespec sitespec.yaml --stack-config stack-config.json --kubeconfig site.kubeconfig \
  --values-for controller=controller-site.yaml --values-for spire=spire-dsx-imds.yaml
```
