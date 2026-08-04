#!/bin/bash

set -e 

NKE_PKG_DIR="${NKE_PKG_DIR:-/var/cache/apt/nke-packages}"
original_dir=$(pwd)
k8s_versions=(
  "1.29.10-1.1"
  "1.29.11-1.1"
  "1.29.12-1.1"
  "1.29.13-1.1"
  "1.29.14-1.1"
  "1.30.10-1.1"
  "1.31.5-1.1"
  "1.31.6-1.1"
)

additional_pkgs=(
  "kubernetes-cni" 
  "conntrack"
  "containerd.io"
  "helm=3.17.1-1"
  "openvswitch-switch"
)

cri_tools_versions=(
  "cri-tools=1.32.0-1.1"
  "cri-tools=1.31.1-1.1"
  "cri-tools=1.30.1-1.1"
  "cri-tools=1.29.0-1.1"
)

function setup_sources() {
  # Setup k8s repos
  curl -fsSL https://pkgs.k8s.io/core:/stable:/v1.32/deb/Release.key | sudo gpg --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg
  echo "deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/v1.29/deb/ /" | sudo tee -a /etc/apt/sources.list.d/kubernetes.list
  echo "deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/v1.30/deb/ /" | sudo tee -a /etc/apt/sources.list.d/kubernetes.list
  echo "deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/v1.31/deb/ /" | sudo tee -a /etc/apt/sources.list.d/kubernetes.list
  echo "deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/v1.32/deb/ /" | sudo tee -a /etc/apt/sources.list.d/kubernetes.list

  # Setup docker repo
  curl -fsSL https://download.docker.com/linux/ubuntu/gpg | gpg --dearmor -o /etc/apt/keyrings/docker.gpg
  echo \
    "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/ubuntu \
    $(lsb_release -cs) stable" | tee -a /etc/apt/sources.list.d/docker.list > /dev/null

  # Sethup helm repo
  curl https://baltocdn.com/helm/signing.asc | gpg --dearmor | tee -a /usr/share/keyrings/helm.gpg > /dev/null
  echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/helm.gpg] https://baltocdn.com/helm/stable/debian/ all main" | tee -a /etc/apt/sources.list.d/helm-stable-debian.list

  # Update the apt cache with our new sources
  apt-get update
}

setup_sources

mkdir -p $NKE_PKG_DIR
cd "$NKE_PKG_DIR" || { echo "Failed to cd to $NKE_PKG_DIR"; exit 1; }

# Loop through the array of k8s versions and download the deb files
for k8s_version in "${k8s_versions[@]}"
do
  echo "Downloading k8s packages for release version $k8s_version"
  apt download kubeadm=$k8s_version kubelet=$k8s_version kubectl=$k8s_version
done

for cri_pkg in "${cri_tools_versions[@]}"
do
  echo "Downloading cri-tools package $cri_pkg"
  apt download $cri_pkg
done

# PUll dependencies and add ons...
for pkg in "${additional_pkgs[@]}"
do
  echo "Downloading additional package $pkg"
  apt download $pkg
done

cd "$original_dir" || { echo "Failed to cd to $original_dir"; exit 1; }
