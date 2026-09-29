# Configuring OVN Kubernetes

Cluster creation accepts optional `ovnKubernetes` settings when `cniPlugin` is
`ovn-kubernetes`. For example, add these fields to a create-cluster request:

```json
{
  "cniPlugin": "ovn-kubernetes",
  "ovnKubernetes": {
    "gatewayInterface": "brfab0",
    "allowNoUplink": true,
    "gatewayMode": "local",
    "mtu": 8900
  }
}
```

All four settings are optional. Omitting the object retains the packaged chart
defaults; existing requests and clusters retain their current behavior. The
settings are stored with the cluster and returned by cluster GET/list operations.
They are immutable after creation, like the CNI selection and cluster CIDRs.

| Setting | Meaning |
|---|---|
| `gatewayInterface` | Interface or existing OVS bridge for the gateway. The same name must exist on every node; physical NIC names and counts can differ when using a common bridge name. NVKE does not create that bridge through this setting. |
| `allowNoUplink` | In `local` gateway mode, allow a bridge without a physical uplink and relax the default-route interface check. It has no effect in `shared` mode. Explicit `false` is preserved. |
| `gatewayMode` | `shared` or `local`. This does not change the default when omitted. |
| `mtu` | Pod MTU in bytes, from 576 to 65535 for NVKE's IPv4 network configuration. Choose a value that fits every node's underlay after encapsulation overhead; NVKE does not discover or resize the host MTU. |

Both installation paths (during kubeadm initialization and after initialization)
use these persisted settings, including installation retries. The existing image
registry override still applies. Kubernetes upgrades do not reinstall the CNI or
change these settings; this API does not introduce a CNI upgrade or repair workflow.

The mapping is verified against the packaged OVN v1.4.0 chart: `gatewayMode` maps
to `global.gatewayMode`, `mtu` maps to `mtu`, and the two gateway options form
`global.gatewayOpts`. Supplying either gateway option replaces that entire chart
option string; omitting both leaves it untouched. This is relevant if an operator
has customized the packaged values. Other chart versions must support these keys.

The v1.4.0 defaults are `shared` mode and pod MTU 1400. Its IPv4 Geneve path needs
58 bytes of overhead, so the example MTU 8900 requires an underlay of at least
8958 bytes. Node-local OVS external IDs such as `ovn-gw-interface` can override
the chart's gateway interface; remove conflicting overrides when using this API.

See the pinned upstream [chart values](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/v1.4.0/helm/ovn-kubernetes/values-single-node-zone.yaml)
and [gateway configuration](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/v1.4.0/go-controller/pkg/config/config.go).
