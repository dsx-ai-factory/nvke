# CNI installation: `InstallCNI` and `ExecuteKubeadmInit`

This note describes **node-agent** behavior for cluster networking: how CNI moved to
`InstallCNI` for **new** orchestration, how **`noCniInstall`** preserves **backward
compatibility with older SWE (orchestrator) versions**, and how that relates to the
legacy in-init CNI path (stand-alone `InstallCNIPlugin` RPC removed).

## `ExecuteKubeadmInit` and `noCniInstall`

`ExecuteKubeadmInit` runs `kubeadm init` and related setup. Whether CNI runs **inside**
that flow depends on **`noCniInstall`** on `KubeadmInitRequest` (default **`false`**
when the field is omitted, matching older clients):

- **`noCniInstall` omitted or `false` (legacy / old SWE):** After a successful init,
  the agent may run the **legacy in-init** path: it maps Tigera/Calico/OVN fields on
  `KubeadmInitRequest` into `InstallCNIPlugin` and calls the same install logic
  (unless `skipCniPluginInstall` is set). This exists so **older NKE / SWE versions**
  that never passed `noCniInstall` continue to work without code changes.
- **`noCniInstall: true` (new SWE / recommended):** No CNI during init. Install
  cluster networking afterward via **`InstallCNI`**.

The CNI-related fields on `KubeadmInitRequest` (manifests, `cni_config`, `ovn_version`,
`cni_plugin_name`, `tigera_version`, etc.) are **used only** when the legacy in-init
path runs (`noCniInstall` not true, and not skipped). New orchestration should set
`noCniInstall: true`, leave those fields empty, and use **`InstallCNI`** only.

## `InstallCNI`: prerequisite debs then charts deploy

Orchestration is responsible for installing Debian packages (typically via **`InstallPackages`**):

| `cni_variant` | Charts/manifest deb (required on CP before **`InstallCNI`**) | Image deb (typically all nodes — optional if registry-pull only) |
|---------------|-----------------------------|------------------|
| OVN           | `nke-cni-ovn-charts-<stream>`   | `nke-cni-ovn-images-<stream>` |
| CALICO        | `nke-cni-calico-manifests-<stream>` | `nke-cni-calico-images-<stream>` |

`cni_variant` **UNSPECIFIED** is treated as **OVN**. **`version`**:strip leading **`v`**; selects **`<stream>`** in the deb naming; empty **`version`** selects defaults matching **cni-packages** (currently OVN **`1.4.0`**, Calico **`3.30.1`**). Pinning still appears as **`=…`** appended to logical package names produced for logging and **`resolvedAptPackage`**.

After the **charts/manifest** deb exists for the chosen **`version`**, **`InstallCNI`** (control-plane role only):

- Verifies **`dpkg`** registers the charts deb (**`FAILED_PRECONDITION`** otherwise).
- **Does not run `apt install`** inside the RPC and **does not validate container images** in containerd — tenants may rely purely on **`imagePullSecrets` / kubelet pulls** against a private registry.
- Runs the same Helm / manifest apply logic as before: **OVN** (`helm install`), **Calico** (Helm vs `kubectl apply` / `apply-manifests.sh` depending on packaging).

Historical proto fields (**`cni_apt_role`**, **`apply_to_cluster`**) are obsolete; **`CNI_APT_ROLE_IMAGES`** is rejected with **`INVALID_ARGUMENT`**. **`skip_apt`** is ignored (previously reserved / unimplemented).

The RPC sets **`resolvedAptPackage`** to the charts/manifest logical package spec constructed from **`cni_variant`** + **`version`**.

## Legacy in-init path (formerly `InstallCNIPlugin`)

`InstallCNIPluginRequest` remains as the internal request shape for in-init CNI; the stand-alone RPC was removed. Prefer `InstallCNI` for stand-alone
calls from **new** code. It remains the implementation behind **legacy in-init CNI**
when `noCniInstall` is not set, for **backward compatibility with old SWE versions**.
**New** orchestration should use **`noCniInstall: true`** on `ExecuteKubeadmInit`,
**`InstallPackages`** for CNI Debian packages where needed (images and/or charts debs),
and **`InstallCNI`** once on a control-plane to apply charts/manifests.

## Summary sequence (orchestrator view)

**New SWE (recommended):**

1. **`ExecuteKubeadmInit`** with **`noCniInstall: true`** on the bootstrap control-plane node.
2. **`InstallPackages`** (**or apt**) for **`nke-cni-*-images-<stream>`** on nodes that must pre-load images (skip if registry-pull-only tenant policy applies).
3. **`InstallPackages`** for **`nke-cni-*-charts-<stream>`** or **`nke-cni-calico-manifests-<stream>`** on **one control-plane**.
4. **`InstallCNI`** once on **that control-plane**.
5. **Join workflow:** continue using **`ExecuteKubeadmJoin`**; repeat **step 2** on join-only nodes where offline image bundles are needed (no **`InstallCNI`** on workers).

**Older SWE (compatibility):** may omit `noCniInstall` and rely on the default in-init
CNI (same effective behavior as pre–`InstallCNI` agents for callers that still populate
the legacy `KubeadmInitRequest` CNI fields).
