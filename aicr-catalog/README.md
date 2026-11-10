# AICR catalog template

The NVKE controller stack as an [AICR](https://github.com/NVIDIA/aicr) data tree. `nvkectl` renders it against a
release description (`release.json`), has `aicr` turn the result into a deploy bundle and runs the bundle's
`deploy.sh`. `nvkectl` holds no chart, values or install order of its own.

| Overlay (`--service`) | Purpose |
|---|---|
| `nvke-core` | Shared parent: Postgres, SPIRE, Temporal and the controller. Not a target. |
| `nvke-seed` | The reduced stack `nvkectl bootstrap site` stands up on its ephemeral cluster, plus the seed's NodePorts. |
| `nvke-site` | The site controller cluster's operational layer: cert-manager, base config, CAPNICo, NKTP and the stack, with `deploy/nico`'s values. |

`registry.yaml` carries the default chart location and version of every component. A release description overrides
the ones it names. A location under `nvke.invalid` has no default home yet and must come from a release.

Values per target live in `components/<name>/{seed,site}-values.yaml`. Site-specific settings (trust domain, TPM
hashes, passwords, images, cluster name) are not here: `nvkectl` supplies them at deploy time.

Cluster API and the provisioned provider are prerequisites outside this catalog.

`check.sh` renders both targets offline and checks the component set (needs `aicr`).
