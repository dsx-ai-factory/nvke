# Chart packaging extensions

`make chart` runs the pinned Kubebuilder Helm plugin in fresh staging and then
packages these extensions. The resulting `chart/` is generated output.

- Change runtime manifests under `config/manager` and permission rules under
  `config/rbac`, `config/dev` or the Go RBAC markers. Generation carries those
  changes into Helm; do not edit the generated Deployment or repeat its defaults.
- Change chart-only installation behavior in `templates/`, `values.json` and
  `values.schema.json` here. These define component switches, target namespaces,
  connection references and the identities that state RBAC binds.
- `../chartgen` applies checked hooks to the plugin output for those choices.
  It leaves probes, resources, security settings and other workload content
  generated from the source manifests. An unexpected hook shape fails generation
  so a plugin upgrade requires review instead of silently dropping behavior.

The plugin preserves existing values by default. Fresh staging regenerates
runtime defaults before adding `values.json`, so source changes remain visible.
Source volumes are already present in the generated template; their duplicated
plugin extra-volume defaults are cleared before site additions are applied.
State schemas and RBAC are packaged from canonical files, with one maintained
copy of each definition.

Run `make chart-check chart-assets-check` to verify generation and installation
modes. The propagation test changes source resources, an environment variable,
a probe, a volume and a permission, then verifies the final Helm rendering.

Created with AI assistance
