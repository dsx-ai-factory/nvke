#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"

# CWE_HTTP_PORT is a deprecated alias for NKE_CONTROLLER_HTTP_PORT; #1399 removes it.
NKE_CONTROLLER_HTTP_PORT="${NKE_CONTROLLER_HTTP_PORT:-${CWE_HTTP_PORT:-8080}}"
API_URL="${API_URL:-http://localhost:${NKE_CONTROLLER_HTTP_PORT}/api}"
CLUSTERS_API_URL="${API_URL%/}/clusters"

CLUSTER_NAME="${CLUSTER_NAME:-$(whoami)-capnico-forge}"
KUBERNETES_VERSION="${KUBERNETES_VERSION:-1.34.2}"
# CNI defaults to ovn-kubernetes. Set CNI_PLUGIN=calico to use Calico instead.
CNI_PLUGIN="${CNI_PLUGIN:-ovn-kubernetes}"
case "${CNI_PLUGIN}" in
  ovn-kubernetes) CNI_IMAGES_PACKAGE="nvke-cni-ovn-images-1.6.0" ;;
  calico) CNI_IMAGES_PACKAGE="nvke-cni-calico-images-3.30.1" ;;
  *)
    printf 'create-forge-nico-cluster: unsupported CNI_PLUGIN %s\n' "${CNI_PLUGIN}" >&2
    exit 1
    ;;
esac
CONTROL_PLANE_DESIRED_COUNT="${CONTROL_PLANE_DESIRED_COUNT:-1}"
WORKER_DESIRED_COUNT="${WORKER_DESIRED_COUNT:-1}"

DEFAULT_SITE_ID="${DEFAULT_SITE_ID:-00000000-0000-0000-0000-000000000000}"

# The site to build in has no default. It used to: four identifiers of a real
# Forge site arrived with the original cloud-workflow-engine import and never
# changed, and nothing records a cluster having been created there -- so they
# were a published map of infrastructure that no longer answered for anything.
#
# Required instead, because a site either exists or it does not and guessing
# one only moves the failure to an API rejection naming a field the caller
# never set. Every missing name is reported at once.
#
# List what a site offers with:
#   ngc --format_type=json forge instance-type list --site "${NICO_SITE_ID}" --org "${NICO_ORG_ID}" | jq
missing=()
for name in NICO_SITE_ID NICO_VPC_ID NICO_SUBNET_ID NICO_SSH_KEY_GROUP_ID NICO_INSTANCE_TYPE_ID; do
  [[ -n "${!name:-}" ]] || missing+=("${name}")
done
if [[ ${#missing[@]} -gt 0 ]]; then
  printf 'create-forge-nico-cluster: set %s\n' "${missing[*]}" >&2
  exit 1
fi

# /api/* is authenticated. hack/nvke-token.sh prints nothing when NVKE is not, so
# this works either way.
TOKEN="$("${SCRIPT_DIR}/nvke-token.sh")"
AUTH_HEADER=()
if [[ -n "${TOKEN}" ]]; then
  AUTH_HEADER=(-H "Authorization: Bearer ${TOKEN}")
fi

BODY=$(cat <<EOF
{
  "name": "${CLUSTER_NAME}",
  "controlPlaneKubernetesVersion": "${KUBERNETES_VERSION}",
  "cniPlugin": "${CNI_PLUGIN}",
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
        "curl -fsSL -o /etc/apt/keyrings/nke.asc https://nke-prod-assets.s3.us-west-2.amazonaws.com/nvke-apt-public.key",
        "echo 'deb [signed-by=/etc/apt/keyrings/nke.asc] https://nke-prod-assets.s3.us-west-2.amazonaws.com/nvke-apt-noble nvke-noble main' > /etc/apt/sources.list.d/nke-apt.list",
        "apt-get update",
        "DEBIAN_FRONTEND=noninteractive apt-get install -y nvke-agent nvke-kubeadm-distro-${KUBERNETES_VERSION}",
        "DEBIAN_FRONTEND=noninteractive apt-get install -y ${CNI_IMAGES_PACKAGE}",
        "DEBIAN_FRONTEND=noninteractive apt-get install -y spire-agent=1.14.1+tpm1.9.0",
        "printf 'AGENT_ARGS=\"--secure-server-port 15005\"\\n' > /etc/default/nvke-agent",
        "sysctl --system",
        "systemctl daemon-reload",
        "systemctl restart nvke-agent.service",
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
        "curl -fsSL -o /etc/apt/keyrings/nke.asc https://nke-prod-assets.s3.us-west-2.amazonaws.com/nvke-apt-public.key",
        "echo 'deb [signed-by=/etc/apt/keyrings/nke.asc] https://nke-prod-assets.s3.us-west-2.amazonaws.com/nvke-apt-noble nvke-noble main' > /etc/apt/sources.list.d/nke-apt.list",
        "apt-get update",
        "DEBIAN_FRONTEND=noninteractive apt-get install -y nvke-agent nvke-kubeadm-distro-${KUBERNETES_VERSION}",
        "DEBIAN_FRONTEND=noninteractive apt-get install -y ${CNI_IMAGES_PACKAGE}",
        "DEBIAN_FRONTEND=noninteractive apt-get install -y spire-agent=1.14.1+tpm1.9.0",
        "printf 'AGENT_ARGS=\"--secure-server-port 15005\"\\n' > /etc/default/nvke-agent",
        "sysctl --system",
        "systemctl daemon-reload",
        "systemctl restart nvke-agent.service",
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
