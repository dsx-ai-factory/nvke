// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package spirehandover

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	helperPod       = "spire-handover-data"
	helperContainer = "helper"
	helperDataDir   = "/data"
	// chownInitContainer is the chart's busybox init container; its image is
	// already on the cluster and has sh, tar and sha256sum.
	chownInitContainer = "chown"
	// serverOwner is the uid:gid the chart runs the SPIRE server as.
	serverOwner = "1000:1000"

	datastoreFile = "datastore.sqlite3"
	keysFile      = "keys.json"
)

// sqliteSidecars are the SQLite journal files that belong to datastoreFile.
var sqliteSidecars = []string{datastoreFile + "-wal", datastoreFile + "-shm"}

// moveState stops both servers and copies the seed server's datastore and
// signing keys onto the site server's volume.
func (h *handover) moveState(ctx context.Context) error {
	// Both servers stop before anything is copied: the site one so it does not
	// write over the copy, the seed one so SQLite is checkpointed and stays put.
	for _, c := range []*cluster{h.site, h.seed} {
		if err := h.scale(ctx, c, 0); err != nil {
			return err
		}
		if err := h.waitNoServerPods(ctx, c); err != nil {
			return err
		}
	}

	defer h.cleanupHelpers(context.WithoutCancel(ctx))
	if err := h.startHelper(ctx, h.seed); err != nil {
		return err
	}
	if err := h.startHelper(ctx, h.site); err != nil {
		return err
	}

	out, err := h.helperExec(ctx, h.seed, "ls", "-1", helperDataDir)
	if err != nil {
		return err
	}
	present := strings.Fields(out)
	files := []string{datastoreFile, keysFile}
	for _, f := range files {
		if !slices.Contains(present, f) {
			return fmt.Errorf("the seed SPIRE volume has no %s", f)
		}
	}
	for _, f := range sqliteSidecars {
		if slices.Contains(present, f) {
			files = append(files, f)
		}
	}

	// Journal files left on the site would be replayed into the copied datastore.
	rm := []string{"rm", "-f"}
	for _, f := range sqliteSidecars {
		rm = append(rm, filepath.Join(helperDataDir, f))
	}
	if _, err := h.helperExec(ctx, h.site, rm...); err != nil {
		return err
	}

	tmp, err := os.MkdirTemp("", "spire-handover-")
	if err != nil {
		return fmt.Errorf("creating a staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	paths := make([]string, 0, len(files))
	for _, f := range files {
		remote := filepath.Join(helperDataDir, f)
		local := filepath.Join(tmp, f)
		if _, err := h.kubectl(ctx, h.seed, "cp", "-c", helperContainer, helperPod+":"+remote, local); err != nil {
			return err
		}
		if _, err := h.kubectl(ctx, h.site, "cp", "-c", helperContainer, local, helperPod+":"+remote); err != nil {
			return err
		}
		paths = append(paths, remote)
	}
	if _, err := h.helperExec(ctx, h.site, append([]string{"chown", serverOwner}, paths...)...); err != nil {
		return err
	}

	seedSums, err := h.checksums(ctx, h.seed, paths)
	if err != nil {
		return err
	}
	siteSums, err := h.checksums(ctx, h.site, paths)
	if err != nil {
		return err
	}
	for _, p := range paths {
		if seedSums[p] == "" || seedSums[p] != siteSums[p] {
			return fmt.Errorf("checksum of %s differs: seed %q, site %q", p, seedSums[p], siteSums[p])
		}
	}
	h.log.Info("copied the SPIRE server state to the site", "files", files)
	return nil
}

func (h *handover) checksums(ctx context.Context, c *cluster, paths []string) (map[string]string, error) {
	out, err := h.helperExec(ctx, c, append([]string{"sha256sum"}, paths...)...)
	if err != nil {
		return nil, err
	}
	sums := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if fields := strings.Fields(line); len(fields) == 2 {
			sums[fields[1]] = fields[0]
		}
	}
	return sums, nil
}

func (h *handover) helperExec(ctx context.Context, c *cluster, cmd ...string) (string, error) {
	return h.kubectl(ctx, c, append([]string{"exec", helperPod, "-c", helperContainer, "--"}, cmd...)...)
}

func (h *handover) scale(ctx context.Context, c *cluster, replicas int) error {
	patch := fmt.Appendf(nil, `{"spec":{"replicas":%d}}`, replicas)
	if _, err := c.kube.AppsV1().StatefulSets(h.cfg.Namespace).Patch(ctx, serverName, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("scaling the %s SPIRE server to %d: %w", c.name, replicas, err)
	}
	h.log.Info("scaled the SPIRE server", "cluster", c.name, "replicas", replicas)
	return nil
}

func (h *handover) poll(ctx context.Context, what string, done func(ctx context.Context) (bool, error)) error {
	if err := wait.PollUntilContextTimeout(ctx, h.cfg.PollInterval, waitTimeout, true, done); err != nil {
		return fmt.Errorf("waiting for %s: %w", what, err)
	}
	return nil
}

func (h *handover) waitNoServerPods(ctx context.Context, c *cluster) error {
	sts, err := h.statefulSet(ctx, c)
	if err != nil {
		return err
	}
	selector, err := metav1.LabelSelectorAsSelector(sts.Spec.Selector)
	if err != nil {
		return fmt.Errorf("the %s SPIRE server selector: %w", c.name, err)
	}
	return h.poll(ctx, c.name+" SPIRE server pods to stop", func(ctx context.Context) (bool, error) {
		pods, err := c.kube.CoreV1().Pods(h.cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
		if err != nil {
			return false, err
		}
		return len(pods.Items) == 0, nil
	})
}

func (h *handover) waitServerReady(ctx context.Context) error {
	return h.poll(ctx, "the site SPIRE server to be ready", func(ctx context.Context) (bool, error) {
		sts, err := h.statefulSet(ctx, h.site)
		if err != nil {
			return false, err
		}
		return sts.Status.ReadyReplicas >= 1, nil
	})
}

// startHelper runs a pod on c that mounts the SPIRE server's data volume, and
// waits for it to run. A helper left by an earlier attempt is replaced.
func (h *handover) startHelper(ctx context.Context, c *cluster) error {
	sts, err := h.statefulSet(ctx, c)
	if err != nil {
		return err
	}
	image := ""
	for _, init := range sts.Spec.Template.Spec.InitContainers {
		if init.Name == chownInitContainer {
			image = init.Image
		}
	}
	if image == "" {
		return fmt.Errorf("the %s SPIRE server has no %s init container to take a helper image from", c.name, chownInitContainer)
	}
	if err := h.deleteHelper(ctx, c); err != nil {
		return err
	}
	root := int64(0)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: helperPod, Labels: map[string]string{"app.kubernetes.io/name": helperPod}},
		Spec: corev1.PodSpec{
			RestartPolicy:    corev1.RestartPolicyNever,
			ImagePullSecrets: sts.Spec.Template.Spec.ImagePullSecrets,
			Tolerations:      []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			Containers: []corev1.Container{{
				Name:    helperContainer,
				Image:   image,
				Command: []string{"sleep", "3600"},
				// Root, to write the files and hand them to the server's uid.
				SecurityContext: &corev1.SecurityContext{RunAsUser: &root, RunAsGroup: &root},
				VolumeMounts:    []corev1.VolumeMount{{Name: "data", MountPath: helperDataDir}},
			}},
			Volumes: []corev1.Volume{{
				Name:         "data",
				VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: dataPVC}},
			}},
		},
	}
	if _, err := c.kube.CoreV1().Pods(h.cfg.Namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("creating the %s SPIRE data helper pod: %w", c.name, err)
	}
	return h.poll(ctx, c.name+" SPIRE data helper pod to run", func(ctx context.Context) (bool, error) {
		p, err := c.kube.CoreV1().Pods(h.cfg.Namespace).Get(ctx, helperPod, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if p.Status.Phase == corev1.PodFailed || p.Status.Phase == corev1.PodSucceeded {
			return false, fmt.Errorf("the %s helper pod ended in phase %s", c.name, p.Status.Phase)
		}
		return p.Status.Phase == corev1.PodRunning, nil
	})
}

func (h *handover) cleanupHelpers(ctx context.Context) {
	for _, c := range []*cluster{h.seed, h.site} {
		if err := h.deleteHelper(ctx, c); err != nil {
			h.log.Warn("leaving the SPIRE data helper pod behind", "cluster", c.name, "pod", helperPod, "err", err)
		}
	}
}

// deleteHelper deletes the helper pod on c, if any, and waits for it to go:
// it holds the data volume, which the server pod needs back.
func (h *handover) deleteHelper(ctx context.Context, c *cluster) error {
	pods := c.kube.CoreV1().Pods(h.cfg.Namespace)
	now := int64(0)
	err := pods.Delete(ctx, helperPod, metav1.DeleteOptions{GracePeriodSeconds: &now})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("deleting the %s SPIRE data helper pod: %w", c.name, err)
	}
	return h.poll(ctx, c.name+" SPIRE data helper pod to go", func(ctx context.Context) (bool, error) {
		_, err := pods.Get(ctx, helperPod, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	})
}
