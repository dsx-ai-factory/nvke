# Access a workload cluster

This page describes how to get the admin kubeconfig for a workload cluster, give the customer access to the cluster, and connect to its Kubernetes API with `kubectl`. The operator does this after the cluster is provisioned, as described in [Create a workload cluster](create.md). If you are the customer and you already have a kubeconfig, start at [Connect with kubectl](#connect-with-kubectl).

## Before you begin

- You have completed [Create a workload cluster](create.md), and `nvkectl get clusters <WORKLOAD_CLUSTER>` shows the cluster with status `Provisioned`.
- The operator has configured `nvkectl` for the management plane with `nvkectl config set-site`, as described in [Install management-plane services](../operate/install-management-plane-services.md). `nvkectl get kubeconfig` reads a Secret on the management cluster with the kubeconfig given to `set-site --kubeconfig`, which must allow `get` on Secrets in the `nke-system` namespace. The management cluster's admin kubeconfig, `<CLUSTER_NAME>.kubeconfig`, allows it. The OIDC kubeconfig from [Secure the NVKE API](../operate/api-authentication.md) does not, because the bootstrap does not configure the management cluster's Kubernetes API server to accept tokens from your identity provider; with API authentication on, use the `kubectl` alternative in step 1.
- The machine where you run `kubectl` has a network route to the cluster's Kubernetes API address on port 6443. See [How clients reach the Kubernetes API](#how-clients-reach-the-kubernetes-api).
- `kubectl` is installed on that machine.

## Get the admin kubeconfig

The operator runs these steps.

1. Download the admin kubeconfig.

   ```bash
   nvkectl get kubeconfig <WORKLOAD_CLUSTER> --output-file <WORKLOAD_CLUSTER>.kubeconfig
   ```

   `<WORKLOAD_CLUSTER>` is the cluster's name as `nvkectl get clusters` lists it. The command prints `kubeconfig written to <WORKLOAD_CLUSTER>.kubeconfig` and creates the file readable only by you. Without `--output-file`, it prints the kubeconfig to standard output.

   `nvkectl` reads the kubeconfig from the `value` key of the `<WORKLOAD_CLUSTER>-admin-kubeconfig` Secret in the `nke-system` namespace of the management cluster. NVIDIA Kubernetes Engine (NVKE) publishes that Secret once a control-plane node has joined. If the command reports that the Secret is not found, check the cluster name, or wait for the cluster to finish provisioning. If it reports that it is not allowed to read the Secret, use a management-cluster kubeconfig with `get` on Secrets in `nke-system`.

   If `nvkectl` uses an OIDC kubeconfig because API authentication is on, read the same Secret with `kubectl` and the management cluster's admin kubeconfig instead:

   ```bash
   (
     set -euo pipefail
     tmp="$(mktemp <WORKLOAD_CLUSTER>.kubeconfig.XXXXXX)"
     trap 'rm -f "$tmp"' EXIT
     kubectl --kubeconfig <CLUSTER_NAME>.kubeconfig -n nke-system get secret <WORKLOAD_CLUSTER>-admin-kubeconfig -o jsonpath='{.data.value}' | base64 -d > "$tmp"
     test -s "$tmp"
     chmod 600 "$tmp"
     mv "$tmp" <WORKLOAD_CLUSTER>.kubeconfig
   )
   ```

   `<CLUSTER_NAME>` is the management cluster's name. The commands decode the kubeconfig into a temporary file readable only by you, and replace `<WORKLOAD_CLUSTER>.kubeconfig` only when `kubectl` and the decode succeed and the result is not empty. On any failure they print the error, delete the temporary file and leave an existing `<WORKLOAD_CLUSTER>.kubeconfig` unchanged.

2. Check the API address the kubeconfig uses.

   ```bash
   kubectl --kubeconfig <WORKLOAD_CLUSTER>.kubeconfig config view --minify -o jsonpath='{.clusters[0].cluster.server}'
   ```

   The output is `https://<API_ADDRESS>:6443`, where `<API_ADDRESS>` is the cluster's `controlPlaneVIP`, or the first control-plane node's IP address if the cluster has no virtual IP address (VIP).

3. Confirm that the kubeconfig works.

   ```bash
   kubectl --kubeconfig <WORKLOAD_CLUSTER>.kubeconfig get nodes
   ```

   The nodes that have joined the cluster are listed. Other nodes can still be joining after the cluster reaches `Provisioned`. If the command times out, your machine has no route to `<API_ADDRESS>`; the download in step 1 only proves access to the management cluster.

The kubeconfig is a secret. It holds a client certificate and private key for the user `kubernetes-admin` in the group `kubeadm:cluster-admins`, which has `cluster-admin` on the workload cluster. Anyone who holds the file and reaches the API address has full control of the cluster. Do not commit it to a repository, paste it into a ticket, or give it to an application.

## Give the customer access

NVKE gives one credential for a workload cluster: the admin kubeconfig. To give the customer access, send them the file through a channel you trust for passwords, and tell them the API address from step 2 so they can arrange a network route to it.

You cannot revoke a copy you hand over. It stays valid until its certificate expires, because the cluster's certificate authority is not rotated. Hand it over only to the people who administer the cluster for the customer, and have them create narrower credentials for everyone else, as described in [What the customer manages](#what-the-customer-manages).

`enableOidc`, described in [Create a workload cluster](create.md), does not connect the customer's identity provider. It makes the cluster's API server accept OpenID Connect (OIDC) tokens from the single issuer configured for the management plane, which the management plane and the Cluster API controllers use to reach the cluster. The admin kubeconfig is still published with OIDC on, and it is still how the customer gets access. NVKE does not issue per-user credentials to customers; see [Preview status and known limitations](../overview/preview-status.md).

## Connect with kubectl

The customer runs these steps with the kubeconfig the operator sent.

1. Store the kubeconfig where only you can read it.

   ```bash
   chmod 600 <WORKLOAD_CLUSTER>.kubeconfig
   ```

2. Check your identity on the cluster.

   ```bash
   kubectl --kubeconfig <WORKLOAD_CLUSTER>.kubeconfig auth whoami
   ```

   The output shows the username `kubernetes-admin` and the group `kubeadm:cluster-admins`.

3. List the nodes.

   ```bash
   kubectl --kubeconfig <WORKLOAD_CLUSTER>.kubeconfig get nodes
   ```

   The nodes that have joined the cluster are listed with status `Ready`. If the command times out, ask your operator how your network reaches the cluster's API address.

Every NVKE workload cluster's admin kubeconfig uses the cluster name `kubernetes` and the context `kubernetes-admin@kubernetes`. If you hold kubeconfigs for more than one cluster, pass each file with `--kubeconfig` instead of merging them, or rename the cluster and context entries when you merge.

## What the customer manages

The customer owns what runs in the cluster: workloads, their images, namespaces, application secrets, and role-based access control (RBAC) for their own users within the limits the operator sets. The admin kubeconfig has `cluster-admin`, so NVKE does not enforce those limits inside the cluster; agree them with the customer.

To give their own users and automation access, the customer creates Kubernetes identities, such as ServiceAccounts with tokens from `kubectl create token`, and binds them to Roles or ClusterRoles that grant only what each needs. NVKE creates no default roles for customer users.

The operator manages the rest through the NVKE API, and the customer asks the operator for changes to it:

- Nodes: the node count, instance type, labels, taints and Kubernetes version of each node pool. See [Manage node pools](node-pools.md).
- The control plane, including the `kube-vip` static Pod that serves the API address, and the cluster's Container Network Interface (CNI) plugin and network settings.
- On clusters with OIDC turned on, the RBAC objects NVKE applies for its own access: `nke:cwe` and `nke:capi-cluster-admin`. Deleting or editing them blocks the management plane from operating the cluster.

## How clients reach the Kubernetes API

Clients connect directly to the address in the kubeconfig. Neither the management plane nor `nvkectl` proxies Kubernetes API traffic.

When the cluster has a `controlPlaneVIP`, every control-plane node runs `kube-vip`, which advertises the VIP over Border Gateway Protocol (BGP) and withdraws it when that node's API server fails its readiness check. Traffic to the VIP on port 6443 reaches a healthy control-plane node. Without a VIP, the API address is the first control-plane node's IP address, and the API is unavailable whenever that node is.

NVKE does not create a public endpoint or an external load balancer for the API. Your network decides who can reach the API address. The management cluster must also reach it on port 6443 to operate the cluster.

## Keep the kubeconfig current

The client certificate in the admin kubeconfig is valid for one year. NVKE checks each workload cluster's control-plane certificates every 12 hours and renews them when less than 30 days remain. Renewal updates the certificates on the control-plane nodes, including the one NVKE publishes, but not copies already downloaded.

To see when your copy expires, run:

```bash
kubectl --kubeconfig <WORKLOAD_CLUSTER>.kubeconfig config view --raw --minify -o jsonpath='{.users[0].user.client-certificate-data}' | base64 -d | openssl x509 -noout -enddate
```

In the last 30 days before that date, after NVKE has renewed the certificates, the operator runs `nvkectl get kubeconfig` again, runs the same check on the new file to confirm a later date, and sends the customer the new file.

## Verify

The customer runs:

```bash
kubectl --kubeconfig <WORKLOAD_CLUSTER>.kubeconfig auth can-i '*' '*' --all-namespaces
```

The output is `yes`.

## Next steps

- [Manage node pools](node-pools.md) to scale the cluster or replace nodes.
- [Observe NVKE and its clusters](../operate/observe.md) to collect metrics and logs from the cluster.
