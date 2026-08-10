# Umbrella chart as an AICR recipe

A mechanical conversion of the internal `nke-site-controller` umbrella Helm
chart into an AICR recipe, so the two composition models can be compared on the
same component set rather than in the abstract.

The umbrella is one Helm chart with 29 dependency entries resolving to 27
unique charts (`cnpg-cluster` appears three times under aliases). It ships as a
single ~2 MB OCI artifact with every subchart vendored inside, and one Argo CD
Application deploys the whole thing.

This directory expresses the same component set as a recipe and renders it.

## Layout

```
data/registry.yaml        29 component definitions
data/overlays/base.yaml   the composition: components, versions,
                          namespaces, dependency edges
data/overlays/forge.yaml  leaf overlay, full stack
data/overlays/forge-coexist.yaml
                          leaf overlay, controller layer only
data/components/*/        per-component values (see Limitations)
render.sh                 regenerates generated/
generated/                the rendered bundle, committed so it can be read
                          without running anything:
  app-of-apps.yaml          root Application
  NNN-<component>/          one Application plus its values, per component
  checksums.txt             closed-world inventory, 95 entries
  data/                     the recipe, copied in by the bundler
  README.md                 written by AICR, not by hand
```

`generated/` is the full stack: 96 files, one root `app-of-apps.yaml` plus 30
child Applications. The recipe declares 29 components; the thirtieth is
`prometheus-adapter`, which the CLI adds on its own (see Limitations). `data/`
appears twice — once as the authored source, once copied inside the bundle.

## Reproduce

```bash
./render.sh                  # rewrites generated/ in place
git diff --stat generated/   # empty means the recipe still produces the committed bundle
aicr verify generated        # checks the bundle against checksums.txt
```

Swap the leaf overlay in `render.sh` to `data/overlays/forge-coexist.yaml` for
the reduced set. That render is not committed; the wave numbers it produces are
quoted below.

## What the conversion shows

**Ordering is derived rather than assigned.** The umbrella carries hand-written
`argocd.argoproj.io/sync-wave` annotations (`-1`, `40`, `50`, `60`) spread
across its values and the per-environment app-of-apps files. The recipe
declares `dependencyRefs`; AICR computes topological levels and assigns waves,
reserving a four-slot band per level. Components in the same level deploy in
parallel:

| wave | components |
|------|------------|
| 1 | cert-manager, external-secrets, fluent-bit, gateway-api-crds, kubernetes-replicator, longhorn, nvault-k8s, reloader, skyhook-operator, spire-crds, teleport-kube-agent, zipkin |
| 5 | cloudnative-pg, cluster-secrets, envoy-gateway, kube-prometheus-stack, nke-skyhooks, nke-spire, nke-storage, nke-teleport-apps, tbot |
| 9 | cnpg-cloud, cnpg-site, cnpg-temporal, dgxc-alloy, l0-nke-monitor, prometheus-adapter |
| 13 | temporal |
| 17 | cloud-workflow-engine, site-workflow-engine |

**Ordering re-derives when the component set changes.** `forge-coexist.yaml`
disables six components and renders 24 child Applications instead of 30. Because
`temporal` and `cloudnative-pg` are gone, the graph shortens on its own:
`cnpg-*` moves from wave 9 to 1, `site-workflow-engine` from 17 to 5,
`cloud-workflow-engine` from 17 to 9, and the five levels collapse to three.
The umbrella's equivalent profile disables the same components but leaves the
sync-wave annotations fixed.

Note the trade: AICR drops a dependency edge when the dependency is disabled,
assuming it is satisfied outside the bundle. That is correct for a
coexistence profile and is an assumption everywhere else.

**Namespace is a property of the deployment, not the chart.** Every component
renders as its own Application with its own `destination.namespace` and
`CreateNamespace=true`. See
`generated/024-teleport-kube-agent/application.yaml`, which lands in
`dgxc-teleport` without the chart doing anything.

This matters because subcharts of a single Helm release share
`.Release.Namespace`. That constraint is why several charts in the umbrella are
carried as `-nke.N` forks whose only change is namespace placement. Under
per-component Applications the constraint does not exist.

The forks are *not* proven unnecessary by this conversion — the recipe still
pins the fork versions, because that is what the umbrella's lock file pins.
Establishing that any given fork can be dropped means substituting the upstream
chart and verifying, per chart, and some were forked for reasons other than
namespace placement.

## Limitations

**The values are stubs.** `data/components/*/values.yaml` are placeholders. The
umbrella's `values.yaml` is ~1,700 lines and transplanting it is the bulk of a
real conversion, not the part shown here.

**There is no `global:` equivalent.** The umbrella uses Helm's global values to
fan settings out to many subcharts at once. A recipe gives each component its
own values file with no shared tree, so those settings become one entry per
consumer with nothing enforcing that they agree. This is the largest open
question in the conversion and this example does not answer it.

**The stock CLI contributes its own overlays.** Rendering logs
`appliedOverlays=[base monitoring-hpa forge]`. `monitoring-hpa` comes from
AICR's embedded catalog, not from this directory, and it adds a component the
recipe never declared (29 in, 30 out). Any real adoption needs to decide
whether the embedded catalog participates.

**Registry coordinates are placeholders.** Chart sources read
`oci://registry.example/nke-charts`. Component names and versions are the real
ones so the set can be checked against the umbrella's lock file; only the
registry location is substituted.

**Dependency edges are hand-written.** They were derived from the umbrella's
sync-wave annotations and its documented ordering constraints, not extracted
mechanically. A production conversion should generate them from the source of
truth.
