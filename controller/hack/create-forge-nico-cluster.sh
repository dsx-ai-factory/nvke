#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"

CWE_HTTP_PORT="${CWE_HTTP_PORT:-8080}"
API_URL="${API_URL:-http://localhost:${CWE_HTTP_PORT}/api}"
CLUSTERS_API_URL="${API_URL%/}/clusters"

CLUSTER_NAME="${CLUSTER_NAME:-$(whoami)-capnico-forge}"
KUBERNETES_VERSION="${KUBERNETES_VERSION:-1.34.2}"
CONTROL_PLANE_DESIRED_COUNT="${CONTROL_PLANE_DESIRED_COUNT:-1}"
WORKER_DESIRED_COUNT="${WORKER_DESIRED_COUNT:-1}"

DEFAULT_SITE_ID="${DEFAULT_SITE_ID:-00000000-0000-0000-0000-000000000000}"
NICO_SITE_ID="${NICO_SITE_ID:-e85ae455-2962-4742-b885-4b9ae22769c8}"
NICO_VPC_ID="${NICO_VPC_ID:-b64210af-8f8e-4e37-ad0a-8cf7c6babf28}"
NICO_SUBNET_ID="${NICO_SUBNET_ID:-53c42b79-3dbf-40e8-9363-75228d7b6e1b}"
NICO_SSH_KEY_GROUP_ID="${NICO_SSH_KEY_GROUP_ID:-5057cbb3-51ba-4568-b9ad-531cdd626d97}"

# Reference:
# ngc --format_type=json forge instance-type list --site e85ae455-2962-4742-b885-4b9ae22769c8 --org novxwn552ssc | jq
#
# gpu.l40x8.ovx.t1.ibx2 - 81873aa9-dbc4-4e7b-95b4-024625c79dad
# gpu.l40x8.ovx.t1 - 7c168e39-8e3b-480f-a3c1-f9b90517bfc9
# stor.1920gb.32x.t1 - a955d89b-8680-4285-8bbf-8338b3874463
# cpu.t1 - d1a6401c-6b19-4261-92c3-79555cb56462
# gpu.l40x8.ovx.t1.ib - 3b7bc0c3-9fd4-49b9-8c08-357d84cdb456
NICO_INSTANCE_TYPE_ID="${NICO_INSTANCE_TYPE_ID:-81873aa9-dbc4-4e7b-95b4-024625c79dad}"

# /api/* is authenticated. hack/nke-token.sh prints nothing when NKE is not, so
# this works either way.
TOKEN="$("${SCRIPT_DIR}/nke-token.sh")"
AUTH_HEADER=()
if [[ -n "${TOKEN}" ]]; then
  AUTH_HEADER=(-H "Authorization: Bearer ${TOKEN}")
fi

BODY=$(cat <<EOF
{
  "name": "${CLUSTER_NAME}",
  "controlPlaneKubernetesVersion": "${KUBERNETES_VERSION}",
  "cniPlugin": "ovn-kubernetes",
  "etcdEncrypted": false,
  "infra": {
    "cluster": {
      "apiVersion": "infrastructure.cluster.x-k8s.io/v1alpha1",
      "kind": "NicoCluster",
      "metadata": {
        "name": "${CLUSTER_NAME}"
      },
      "spec": {
        "siteID": "${NICO_SITE_ID}"
      }
    }
  },
  "nodePools": [
    {
      "name": "control-plane",
      "nodeNamePrefix": "cp",
      "systemReservedMemory": "16Gi",
      "desiredNodeCount": ${CONTROL_PLANE_DESIRED_COUNT},
      "controlPlane": true,
      "autoProvisionDisabled": false,
      "autoModeDisabled": false,
      "recycleBudget": 0,
      "infra": {
        "machineTemplate": {
        "apiVersion": "infrastructure.cluster.x-k8s.io/v1alpha1",
        "kind": "NicoMachineTemplate",
        "metadata": {
          "name": "${CLUSTER_NAME}-control-plane-template"
        },
        "spec": {
          "template": {
            "spec": {
              "vpcID": "${NICO_VPC_ID}",
              "instanceTypeID": "${NICO_INSTANCE_TYPE_ID}",
              "cloudInitInjectHostname": true,
              "ipxeScript": "#!ipxe\nset boot-loader https://d3mq564cn7sth7.cloudfront.net/nke-isos/qcow/efi/x86_64/2026-05-06/1778078637/qcow-imager.efi\nset image-url https://d3mq564cn7sth7.cloudfront.net/nke-isos/qcow/images/24.04/x86_64/lvm/2026-05-06/778/nke-24.04-x86_64-lvm-2026-05-06-778.qcow2\nset disk-strategy smallest\n\n# You probably don't need to change these\nset console-config console=tty0 console=ttyS1,115200\nset kernel-params pci=realloc=off reboot=pci loglevel=1 iommu=pt intel_iommu=on\nset luks-key luks.key=changeit\nset cloud-init-ds ds=nocloud-net;s=\${cloudinit-url}\n\nchain \${boot-loader} \${console-config} \${kernel-params} \${luks-key} image_url=\${image-url} disk_strategy=\${disk-strategy} \${cloud-init-ds} update_grub_template=yes update_grub_cfg=yes\nboot\n",
              "sshKeyGroupIDs": [
                "${NICO_SSH_KEY_GROUP_ID}"
              ],
              "interfaces": [
                {
                  "subnetID": "${NICO_SUBNET_ID}"
                }
              ]
            }
          }
        }
        },
        "preKubeadmCommands": [
        "install -d -m 0755 /etc/apt/keyrings",
        "curl -fsSL -o /etc/apt/keyrings/nke.asc https://nke-prod-assets.s3.us-west-2.amazonaws.com/nke-apt-public.key",
        "echo 'deb [signed-by=/etc/apt/keyrings/nke.asc] https://nke-prod-assets.s3.us-west-2.amazonaws.com/nke-apt-noble nke-noble main' > /etc/apt/sources.list.d/nke-apt.list",
        "apt-get update",
        "DEBIAN_FRONTEND=noninteractive apt-get install -y nke-agent nke-kubeadm-distro-${KUBERNETES_VERSION}",
        "DEBIAN_FRONTEND=noninteractive apt-get install -y nke-cni-ovn-images-1.4.0",
        "DEBIAN_FRONTEND=noninteractive apt-get install -y spire-agent=1.14.1+tpm1.9.0",
        "printf 'AGENT_ARGS=\"--secure-server-port 15005\"\\n' > /etc/default/nke-agent",
        "sysctl --system",
        "systemctl daemon-reload",
        "systemctl restart nke-agent.service",
        "systemctl restart procps",
        "touch /run/cloud-init.done"
      ]
      }
    },
    {
      "name": "worker",
      "nodeNamePrefix": "w",
      "systemReservedMemory": "16Gi",
      "desiredNodeCount": ${WORKER_DESIRED_COUNT},
      "controlPlane": false,
      "autoProvisionDisabled": false,
      "autoModeDisabled": false,
      "recycleBudget": 0,
      "infra": {
        "machineTemplate": {
        "apiVersion": "infrastructure.cluster.x-k8s.io/v1alpha1",
        "kind": "NicoMachineTemplate",
        "metadata": {
          "name": "${CLUSTER_NAME}-worker-template"
        },
        "spec": {
          "template": {
            "spec": {
              "vpcID": "${NICO_VPC_ID}",
              "instanceTypeID": "${NICO_INSTANCE_TYPE_ID}",
              "cloudInitInjectHostname": true,
              "ipxeScript": "#!ipxe\nset boot-loader https://d3mq564cn7sth7.cloudfront.net/nke-isos/qcow/efi/x86_64/2026-05-06/1778078637/qcow-imager.efi\nset image-url https://d3mq564cn7sth7.cloudfront.net/nke-isos/qcow/images/24.04/x86_64/lvm/2026-05-06/778/nke-24.04-x86_64-lvm-2026-05-06-778.qcow2\nset disk-strategy smallest\n\n# You probably don't need to change these\nset console-config console=tty0 console=ttyS1,115200\nset kernel-params pci=realloc=off reboot=pci loglevel=1 iommu=pt intel_iommu=on\nset luks-key luks.key=changeit\nset cloud-init-ds ds=nocloud-net;s=\${cloudinit-url}\n\nchain \${boot-loader} \${console-config} \${kernel-params} \${luks-key} image_url=\${image-url} disk_strategy=\${disk-strategy} \${cloud-init-ds} update_grub_template=yes update_grub_cfg=yes\nboot\n",
              "sshKeyGroupIDs": [
                "${NICO_SSH_KEY_GROUP_ID}"
              ],
              "interfaces": [
                {
                  "subnetID": "${NICO_SUBNET_ID}"
                }
              ]
            }
          }
        }
        },
        "preKubeadmCommands": [
        "install -d -m 0755 /etc/apt/keyrings",
        "curl -fsSL -o /etc/apt/keyrings/nke.asc https://nke-prod-assets.s3.us-west-2.amazonaws.com/nke-apt-public.key",
        "echo 'deb [signed-by=/etc/apt/keyrings/nke.asc] https://nke-prod-assets.s3.us-west-2.amazonaws.com/nke-apt-noble nke-noble main' > /etc/apt/sources.list.d/nke-apt.list",
        "apt-get update",
        "DEBIAN_FRONTEND=noninteractive apt-get install -y nke-agent nke-kubeadm-distro-${KUBERNETES_VERSION}",
        "DEBIAN_FRONTEND=noninteractive apt-get install -y nke-cni-ovn-images-1.4.0",
        "DEBIAN_FRONTEND=noninteractive apt-get install -y spire-agent=1.14.1+tpm1.9.0",
        "printf 'AGENT_ARGS=\"--secure-server-port 15005\"\\n' > /etc/default/nke-agent",
        "sysctl --system",
        "systemctl daemon-reload",
        "systemctl restart nke-agent.service",
        "systemctl restart procps",
        "touch /run/cloud-init.done"
      ]
      }
    }
  ]
}
EOF
)

curl "${AUTH_HEADER[@]+"${AUTH_HEADER[@]}"}" -sS "${CLUSTERS_API_URL}" \
  -H "Content-Type: application/json" \
  --data-raw "${BODY}" | jq
