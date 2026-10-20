// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package artifactdiagnostic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/nvke/controller/api/testutil/health"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllerconfig"
)

func (r *runner) kube(ctx context.Context, args ...string) ([]byte, error) {
	prefix := make([]string, 0, 3+len(args))
	prefix = append(prefix, "--kubeconfig", filepath.Join(r.options.Output, "kubeconfig"), "--request-timeout=30s")
	return r.command(ctx, nil, "kubectl", append(prefix, args...)...)
}

func (r *runner) apply(ctx context.Context, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = r.command(ctx, bytes.NewReader(raw), "kubectl", "--kubeconfig", filepath.Join(r.options.Output, "kubeconfig"), "--request-timeout=30s", "apply", "-f", "-")
	return err
}

func (r *runner) controller(ctx context.Context) (result error) {
	index, manifest, config, err := inspectImage(ctx, r.options.Subject)
	if err != nil {
		return err
	}
	r.report.IndexDigest = index
	r.report.ManifestDigest = manifest
	r.report.ConfigDigest = config
	r.report.Dependencies = []string{r.options.NodeImage, r.options.PostgresImage, r.options.TemporalImage}
	// Check daemon architecture, never relying on a --platform flag to prove native execution.
	host, err := r.command(ctx, nil, "docker", "info", "--format", "{{.OSType}}/{{.Architecture}}")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(host)) != "linux/x86_64" && strings.TrimSpace(string(host)) != nativePlatform {
		return errors.New("docker daemon must be native Linux AMD64")
	}
	names, err := r.command(ctx, nil, "docker", "ps", "-a", "--filter", "name=^/"+r.options.Cluster+"-control-plane$", "--format", "{{.Names}}")
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(names)) != 0 {
		return errors.New("cluster name already owned")
	}
	networks, err := r.command(ctx, nil, "docker", "network", "ls", "--filter", "name=^"+r.options.Cluster+"$", "--format", "{{.ID}}")
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(networks)) != 0 {
		return errors.New("network name already owned")
	}
	// Register before create so partial creation also has bounded teardown.
	if err := os.WriteFile(filepath.Join(r.options.Output, "owned-cluster"), []byte(r.options.Cluster), 0o600); err != nil {
		return err
	}
	r.report.Cleanup = "pending"
	defer func() { result = errors.Join(result, r.cleanupController(ctx)) }()
	if err := r.prepareController(ctx); err != nil {
		return err
	}
	return r.startController(ctx, manifest, config)
}

func (r *runner) prepareController(ctx context.Context) error {
	r.report.Phase = "prepare"
	if _, err := r.command(ctx, nil, "docker", "network", "create", r.options.Cluster); err != nil {
		return err
	}
	if _, err := r.command(ctx, nil, kindTool, "create", "cluster", "--name", r.options.Cluster, "--image", r.options.NodeImage, "--kubeconfig", filepath.Join(r.options.Output, "kubeconfig"), "--wait", "120s"); err != nil {
		return err
	}
	if _, err := r.kube(ctx, "create", "namespace", "iv2"); err != nil {
		return err
	}
	if _, err := r.kube(ctx, "apply", "-f", "deploy/charts/controller/files/crds.yaml"); err != nil {
		return err
	}
	capi, err := r.command(ctx, nil, "go", "list", "-m", "-f", "{{.Dir}}", "sigs.k8s.io/cluster-api")
	if err != nil {
		return err
	}
	if _, err := r.kube(ctx, "apply", "-f", filepath.Join(strings.TrimSpace(string(capi)), "config", "crd", "bases", "cluster.x-k8s.io_clusters.yaml")); err != nil {
		return err
	}
	if _, err := r.kube(ctx, "wait", "--for=condition=Established", "--timeout=60s", "crd", "--all"); err != nil {
		return err
	}
	cfg, err := controllerconfig.Load("hack/local-controller-config.yaml")
	if err != nil {
		return err
	}
	cfg.Temporal.HostPort = "temporal:7233"
	cfg.ManagementNamespace = "iv2"
	cfg.ProfilesDir = "/profiles"
	cfg.RunbooksDir = ""
	cfg.NicoControllersEnabled = false
	cfg.CertRenewal.ControllerEnabled = false
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := r.apply(ctx, map[string]any{"apiVersion": "v1", kindTool: "ConfigMap", "metadata": map[string]string{"name": controllerConfigName, "namespace": "iv2"}, "data": map[string]string{"config.yaml": string(raw)}}); err != nil {
		return err
	}
	for _, service := range []struct {
		name, image string
		args        []string
		port        int
		env         []corev1.EnvVar
	}{
		{postgresName, r.options.PostgresImage, nil, 5432, []corev1.EnvVar{{Name: "POSTGRES_HOST_AUTH_METHOD", Value: "trust"}}},
		{temporalName, r.options.TemporalImage, []string{"server", "start-dev", "--ip", "0.0.0.0", "--namespace", "cloud-workflow-engine"}, 7233, nil},
	} {
		pod := fixturePod(service.name, service.image)
		container := &pod.Spec.Containers[0]
		if service.args != nil {
			container.Args = service.args
		}
		if service.env != nil {
			container.Env = service.env
		}
		if err := r.apply(ctx, pod); err != nil {
			return err
		}
		if _, err := r.kube(ctx, "-n", "iv2", "expose", "pod", service.name, "--port", fmt.Sprint(service.port)); err != nil {
			return err
		}
	}
	for _, dependency := range []struct {
		name string
		args []string
	}{
		{postgresName, []string{"pg_isready", "-U", postgresName}},
		{temporalName, []string{temporalName, "operator", "namespace", "describe", "--address", "localhost:7233", "--namespace", "cloud-workflow-engine"}},
	} {
		if err := r.await(ctx, func() error {
			_, err := r.kube(ctx, append([]string{"-n", "iv2", "exec", dependency.name, "--"}, dependency.args...)...)
			return err
		}); err != nil {
			return err
		}
	}
	return nil
}

func (r *runner) startController(ctx context.Context, manifest, config string) error {
	f, err := os.Open(r.options.Subject)
	if err != nil {
		return err
	}
	_, importErr := r.command(ctx, f, "docker", "exec", "-i", r.options.Cluster+"-control-plane", "ctr", "-n", "k8s.io", "images", "import", "--digests", "--platform", nativePlatform, "--base-name", "docker.io/library/iv2-controller", "-")
	_ = f.Close()
	if importErr != nil {
		return importErr
	}
	ref := "docker.io/library/iv2-controller@" + manifest
	pod := fixturePod("controller", ref)
	spec := &pod.Spec
	container := &spec.Containers[0]
	container.ImagePullPolicy = corev1.PullNever
	container.Env = []corev1.EnvVar{{Name: "CONTROLLER_CONFIG_PATH", Value: "/config/config.yaml"}, {Name: "DB_CONNECTION_STRING", Value: "postgres://postgres@postgres:5432/postgres?sslmode=disable"}, {Name: "POD_NAMESPACE", Value: "iv2"}}
	container.VolumeMounts = []corev1.VolumeMount{{Name: "config", MountPath: "/config", ReadOnly: true}}
	spec.Volumes = []corev1.Volume{{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: controllerConfigName}}}}}

	// The fixture's short-lived service account is confined to the owned disposable cluster.
	if _, err := r.kube(ctx, "create", "clusterrolebinding", "iv2-controller", "--clusterrole=cluster-admin", "--serviceaccount=iv2:default"); err != nil {
		return err
	}
	if err := r.apply(ctx, pod); err != nil {
		return err
	}
	r.report.Phase = "startup"
	if err := r.await(ctx, func() error { return r.observe(ctx, manifest, config) }); err != nil {
		return err
	}
	r.report.Checks = append(r.report.Checks, "startup", "native-subject")
	for _, path := range []string{"/healthz", "/readyz", "/api/version"} {
		probe := func() error {
			body, err := r.kube(ctx, "get", "--raw", "/api/v1/namespaces/iv2/pods/controller:8080/proxy"+path)
			if err != nil {
				return err
			}
			return health.Check(path, 200, body, r.options.Version)
		}
		var err error
		if path == "/api/version" {
			err = probe()
		} else {
			err = r.await(ctx, probe)
		}
		if err != nil {
			return err
		}
		r.report.Checks = append(r.report.Checks, path)
	}
	if err := r.observe(ctx, manifest, config); err != nil {
		return err
	}
	r.report.Version = r.options.Version
	return nil
}

func fixturePod(name, image string) *corev1.Pod {
	return &corev1.Pod{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}, ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "iv2", Labels: map[string]string{"app": name}}, Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, NodeSelector: map[string]string{"kubernetes.io/arch": nativeArch, "kubernetes.io/os": nativeOS}, Containers: []corev1.Container{{Name: name, Image: image, ImagePullPolicy: corev1.PullIfNotPresent}}}}
}

func (r *runner) await(ctx context.Context, check func() error) error {
	for {
		err := check()
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w; last observation: %w", ctx.Err(), err)
		case <-time.After(time.Second):
		}
	}
}

func (r *runner) observe(ctx context.Context, manifest, config string) error {
	raw, err := r.kube(ctx, "-n", "iv2", "get", "pod", "controller", "-o", "json")
	if err != nil {
		return err
	}
	var pod struct {
		Spec struct {
			NodeName string `json:"nodeName"`
		} `json:"spec"`
		Metadata struct {
			UID string `json:"uid"`
		} `json:"metadata"`
		Status struct {
			ContainerStatuses []struct {
				ContainerID string `json:"containerID"`
				State       struct {
					Running *json.RawMessage `json:"running"`
				} `json:"state"`
			} `json:"containerStatuses"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &pod); err != nil {
		return err
	}
	if len(pod.Status.ContainerStatuses) != 1 || pod.Status.ContainerStatuses[0].State.Running == nil {
		return errors.New("controller is not running")
	}
	nodeRaw, err := r.kube(ctx, "get", "node", pod.Spec.NodeName, "-o", "json")
	if err != nil {
		return err
	}
	var node struct {
		Status struct {
			NodeInfo struct {
				OS   string `json:"operatingSystem"`
				Arch string `json:"architecture"`
			} `json:"nodeInfo"`
		} `json:"status"`
	}
	if err := json.Unmarshal(nodeRaw, &node); err != nil {
		return err
	}
	platform := node.Status.NodeInfo.OS + "/" + node.Status.NodeInfo.Arch
	if platform != nativePlatform {
		return errors.New("observed node platform is not native Linux AMD64")
	}
	r.report.NodeName = pod.Spec.NodeName
	r.report.NodePlatform = platform
	id := strings.TrimPrefix(pod.Status.ContainerStatuses[0].ContainerID, "containerd://")
	if id == "" {
		return errors.New("missing runtime container identity")
	}
	observed, err := r.command(ctx, nil, "docker", "exec", r.options.Cluster+"-control-plane", "crictl", "inspect", id)
	if err != nil {
		return err
	}
	var status struct {
		Status struct {
			ImageRef string `json:"imageRef"`
		} `json:"status"`
	}
	if err := json.Unmarshal(observed, &status); err != nil {
		return err
	}
	imageRef := status.Status.ImageRef
	digest := imageRef
	if _, after, ok := strings.Cut(imageRef, "@"); ok {
		digest = after
	}
	// Some CRI versions report the config digest. Tie it back through inspecti's repo digests.
	if digest == config {
		image, err := r.command(ctx, nil, "docker", "exec", r.options.Cluster+"-control-plane", "crictl", "inspecti", imageRef)
		if err != nil {
			return err
		}
		var info struct {
			Status struct {
				ID          string   `json:"id"`
				RepoDigests []string `json:"repoDigests"`
			} `json:"status"`
		}
		if err := json.Unmarshal(image, &info); err != nil {
			return err
		}
		matched := false
		for _, ref := range info.Status.RepoDigests {
			if ref == "docker.io/library/iv2-controller@"+manifest {
				matched = true
			}
		}
		if info.Status.ID != config || !matched {
			return errors.New("runtime config is not bound to selected manifest")
		}
	} else if digest != manifest {
		return errors.New("runtime image differs from supplied native manifest")
	}
	r.report.RuntimeImage = imageRef
	r.report.ContainerID = id
	r.report.PodUID = pod.Metadata.UID
	return nil
}

func (r *runner) cleanupController(ctx context.Context) error {
	var result error
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
	defer cancel()
	_, _ = r.kube(cleanup, "-n", "iv2", "logs", "controller", "--timestamps")
	_, _ = r.kube(cleanup, "-n", "iv2", "get", "events", "-o", "json")
	volumes, volumeErr := r.nodeVolumes(cleanup)
	_, deleteErr := r.command(cleanup, nil, kindTool, "delete", "cluster", "--name", r.options.Cluster)
	remaining, observeErr := r.command(cleanup, nil, "docker", "ps", "-a", "--filter", "label=io.x-k8s.kind.cluster="+r.options.Cluster, "--format", "{{.ID}}")
	_, networkErr := r.command(cleanup, nil, "docker", "network", "rm", r.options.Cluster)
	if err := errors.Join(deleteErr, observeErr, networkErr, volumeErr, r.checkVolumesGone(cleanup, volumes)); err != nil || len(bytes.TrimSpace(remaining)) != 0 {
		r.report.Cleanup = cleanupFailed
		r.report.CleanupFailure = fmt.Sprintf("cluster teardown incomplete: %v", err)
		result = errors.Join(result, errors.New(r.report.CleanupFailure))
	} else {
		r.report.Cleanup = complete
		if err := os.Remove(filepath.Join(r.options.Output, "kubeconfig")); err != nil && !os.IsNotExist(err) {
			r.report.Cleanup = cleanupFailed
			r.report.CleanupFailure = "removing private kubeconfig"
			result = errors.Join(result, err)
		}
		if r.report.Cleanup == complete {
			_ = os.Remove(filepath.Join(r.options.Output, "owned-cluster"))
		}
	}
	return result
}

func (r *runner) nodeVolumes(ctx context.Context) ([]string, error) {
	nodes, err := r.command(ctx, nil, "docker", "ps", "-a", "--filter", "label=io.x-k8s.kind.cluster="+r.options.Cluster, "--format", "{{.Names}}")
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(nodes)) == 0 {
		return nil, nil
	}
	mounts, err := r.command(ctx, nil, "docker", "inspect", r.options.Cluster+"-control-plane", "--format", "{{json .Mounts}}")
	if err != nil {
		return nil, err
	}
	var entries []struct {
		Type string `json:"Type"`
		Name string `json:"Name"`
	}
	if err := json.Unmarshal(mounts, &entries); err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if entry.Type == "volume" {
			names = append(names, entry.Name)
		}
	}
	return names, nil
}

func (r *runner) checkVolumesGone(ctx context.Context, names []string) error {
	for _, name := range names {
		remaining, err := r.command(ctx, nil, "docker", "volume", "ls", "--filter", "name=^"+name+"$", "--format", "{{.Name}}")
		if err != nil {
			return err
		}
		if len(bytes.TrimSpace(remaining)) != 0 {
			return fmt.Errorf("owned node volume remains: %s", name)
		}
	}
	return nil
}
