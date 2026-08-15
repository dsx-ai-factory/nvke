# Happy-Case CWE Delete RST Timeline

## Scope

Target deleted by CWE API: `capd-encrypted-cp-0899` (`172.18.0.13`)

Worker observed: `capd-encrypted-w-7606` (`172.18.0.11`)

VIP: `192.168.100.1:6443`

## Baseline

Before deletion, worker VIP sockets were:

- `172.18.0.11:40564 -> 192.168.100.1:6443`: `kube-proxy`, pid `602`, fd `14`
- `172.18.0.11:40604 -> 192.168.100.1:6443`: `kubelet`, pid `377`, fd `25`

Target CP server-side sockets showed both were accepted by `kube-apiserver`, pid `637`.

## Timeline

- `23:46:44`: CWE `DELETE /api/nodes/a4fa253b-b396-4469-9278-8bfbecec2fe8` returned `202 Accepted`.
- `23:46:49.375995`: node-agent received `StartK8SControlPlaneNodeGracefulShutdown`.
- `23:46:49.377812`: node-agent resolved kube-vip VIP `192.168.100.1` from `/etc/kubernetes/manifests/kube-vip.yaml`.
- `23:46:49.389726`: node-agent moved `/etc/kubernetes/manifests/kube-vip.yaml` to `/etc/kubernetes/manifests-backup/kube-vip.yaml`.
- `23:46:50.413422`: node-agent observed the kube-vip container was no longer running.
- `23:46:50`: FRR route view changed from 3 VIP paths (`172.18.0.4`, `172.18.0.10`, `172.18.0.13`) to 2 paths (`172.18.0.4`, `172.18.0.10`), so `capd-encrypted-cp-0899` was withdrawn.
- `23:46:50.418486`: node-agent ran `ss -K` for `172.18.0.11:40604`.
- `23:46:50.420327`: `capd-encrypted-cp-0899` emitted `192.168.100.1:6443 > 172.18.0.11:40604 Flags [R.]`.
- `23:46:50.420342`: worker received that RST. This was the kubelet socket.
- `23:46:50.433608`: node-agent ran `ss -K` for `172.18.0.11:40564`.
- `23:46:50.435570`: `capd-encrypted-cp-0899` emitted `192.168.100.1:6443 > 172.18.0.11:40564 Flags [R.]`.
- `23:46:50.435584`: worker received that RST. This was the kube-proxy socket.
- `23:46:50.438309`: node-agent removed the VIP from `lo`.
- `23:46:51`: worker `ss` showed rebuilt long-lived VIP sockets:
  - `172.18.0.11:60946 -> 192.168.100.1:6443`: `kubelet`, pid `377`, fd `25`
  - `172.18.0.11:32814 -> 192.168.100.1:6443`: `kube-proxy`, pid `602`, fd `14`
- `23:47:20.600478`: node-agent reported `no new remote VIP sockets observed` after the 30s quiet window.
- `23:47:30.431208`: SWE/node-agent moved on to `ExecuteKubeadmReset`.
- `23:47:31.562782`: kubeadm reset RPC completed successfully.

## Impact

The deliberate `ss -K` RSTs were sent by the draining CP node (`capd-encrypted-cp-0899`) kernel, triggered by node-agent. They targeted:

- kubelet old socket `40604`
- kube-proxy old socket `40564`

The worker rebuilt both sockets within the next 1-second `ss` polling sample. Worker kubelet and kube-proxy logs had no entries in the immediate RST window; both files only contain the capture start marker.

Worker tcpdump also captured a short burst of additional worker-initiated VIP connections immediately after the first RST. Those were not the stable kubelet/kube-proxy sockets tracked above; the stable replacements were `60946` and `32814`.

## Comparison With Previous Capture

The new run matches the previous result:

- In both runs, node-agent waited for kube-vip exit before closing VIP sockets.
- In both runs, target CP tcpdump showed VIP-sourced `RST` packets emitted by the draining CP immediately after `ss -K`.
- In both runs, worker tcpdump saw the same `RST` packets.
- In both runs, worker `ss` showed kubelet and kube-proxy rebuilt VIP sockets by the next 1-second polling sample.
- In both runs, the 30s quiet window completed successfully before kubeadm reset.
- In both runs, kubelet/kube-proxy logs showed no immediate user-visible log impact from the RST event.

Differences:

- Previous run target/VIP: `capd-cp-2991`, `192.168.100.0:6443`.
- New run target/VIP: `capd-encrypted-cp-0899`, `192.168.100.1:6443`.
- Previous old sockets: kubelet `37776`, kube-proxy `37972`.
- New old sockets: kubelet `40604`, kube-proxy `40564`.
- New run captured more transient worker-initiated VIP connections during reconnect, but the long-lived kubelet/kube-proxy behavior was the same.
