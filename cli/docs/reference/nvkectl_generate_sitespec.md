> **Generated file -- do not edit.** This page is written from the nvkectl
> command tree in `cli/internal/nvkectl`. To change what it says, change the
> command's own metadata -- its Short, Long, Example, Args, Aliases, deprecation
> notice or flag usage strings -- and run `make -C cli docs`. An edit made here
> is lost on the next run, and `make -C cli docs-check` fails until the
> committed pages match the tree again.

## nvkectl generate sitespec

Generate a site spec file for a tier-0 cluster

### Required options

`--nodes`

### Synopsis

Write a complete site spec for "nvkectl bootstrap site" to standard output.

The hosts come from a CSV so a real site can be described without repeating a
flag per machine. The output needs no editing: every field is validated before
anything is written, so a file this command produces is a file bootstrap site
accepts.

The CSV needs a header naming its columns, in any order. name and ip are
required; tpm_ek_thumbprint is optional per row and enables TPM attestation for
that host.

```
name,ip,tpm_ek_thumbprint
cp-01,10.10.50.2,0000000000000000000000000000000000000000000000000000000000000000
cp-02,10.10.50.3,
cp-03,10.10.50.4,
```

Anything the CSV does not cover is set with --set, using the same field paths
the file itself uses. Run with an unknown path to list them.

The generated file depends only on the arguments given: unlike bootstrap site,
this command ignores the inventory environment variables, so a site spec never
carries a value nobody typed.

```
nvkectl generate sitespec <cluster-name> [flags]
```

### Examples

```
  # Generate a site spec and bootstrap from it
  nvkectl generate sitespec my-tier0-site --nodes hosts.csv \
    --set cluster.controlPlaneVIP=10.10.50.10 > sitespec.yaml
  nvkectl bootstrap site -f sitespec.yaml

  # Override fields the CSV does not carry
  nvkectl generate sitespec my-tier0-site --nodes hosts.csv \
    --set cluster.controlPlaneVIP=10.10.50.10 \
    --set cluster.podCidr=10.244.0.0/16 > sitespec.yaml
```

### Options

```
  -h, --help              help for sitespec
      --nodes string      Path to the node CSV, or '-' to read standard input (required)
      --set stringArray   Set a field, as path=value. Repeatable. Accepts: cluster.cniName, cluster.cniVersion, cluster.controlPlaneVIP, cluster.etcdEncryption, cluster.k8sVersion, cluster.kubeletConfig.systemReservedMemory, cluster.nodeAgentPort, cluster.podCidr, cluster.serviceCidr, clusterName, spire.trustDomain, spire.upstreamCA
```

### Options inherited from parent commands

```
      --json-logs     Emit logs as JSON
      --site string   Override the active site for this invocation
  -v, --verbose       Enable debug logging
```

### SEE ALSO

* [nvkectl generate](nvkectl_generate.md)	 - Generate NVKE configuration files

