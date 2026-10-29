// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
)

var (
	clientset         kubernetes.Interface
	clientsetOnce     sync.Once
	errClientset      error
	overrideClientset kubernetes.Interface // set via SetClientsetForTest in tests
)

// SetClientsetForTest substitutes the clientset returned by getK8sClientSet.
// Pass nil to use the default client built from the admin kubeconfig.
func SetClientsetForTest(cs kubernetes.Interface) {
	overrideClientset = cs
}

func getK8sClientSet() (kubernetes.Interface, error) {
	if overrideClientset != nil {
		return overrideClientset, nil
	}
	clientsetOnce.Do(func() {
		kubeconfig, err := os.ReadFile(AdminKubeconfigPath)
		if err != nil {
			errClientset = err
			return
		}
		config, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
		if err != nil {
			errClientset = err
			return
		}
		clientset, errClientset = kubernetes.NewForConfig(config)
	})
	return clientset, errClientset
}

func updateObjectMetadata(getObj func() (metav1.Object, error), updateObj func(metav1.Object) error, kvStr string, isLabel, remove bool) error {
	kv := strings.SplitN(kvStr, "=", 2)
	if len(kv) < 1 {
		return nil
	}
	key := kv[0]
	val := ""
	if len(kv) == 2 {
		val = kv[1]
	}

	// Other clients (the kubelet, controllers) write the object too; a write between the read and the update
	// makes the update conflict, so re-read and apply the change again.
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		obj, err := getObj()
		if err != nil {
			return err
		}

		if isLabel {
			labels := obj.GetLabels()
			if labels == nil {
				labels = map[string]string{}
			}
			if remove {
				delete(labels, key)
			} else {
				labels[key] = val
			}
			obj.SetLabels(labels)
		} else {
			annotations := obj.GetAnnotations()
			if annotations == nil {
				annotations = map[string]string{}
			}
			if remove {
				delete(annotations, key)
			} else {
				annotations[key] = val
			}
			obj.SetAnnotations(annotations)
		}

		return updateObj(obj)
	})
}

func AnnotatePod(ctx context.Context, namespace, name, annotation string) error {
	client, err := getK8sClientSet()
	if err != nil {
		return err
	}
	return updateObjectMetadata(
		func() (metav1.Object, error) {
			return client.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		},
		func(obj metav1.Object) error {
			pod, ok := obj.(*v1.Pod)
			if !ok {
				return fmt.Errorf("unexpected object type %T, want *v1.Pod", obj)
			}
			_, err := client.CoreV1().Pods(namespace).Update(ctx, pod, metav1.UpdateOptions{})
			return err
		},
		annotation, false, false,
	)
}

func AnnotateNode(ctx context.Context, nodeName, annotation string) error {
	client, err := getK8sClientSet()
	if err != nil {
		return err
	}
	return updateObjectMetadata(
		func() (metav1.Object, error) {
			return client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		},
		func(obj metav1.Object) error {
			node, ok := obj.(*v1.Node)
			if !ok {
				return fmt.Errorf("unexpected object type %T, want *v1.Node", obj)
			}
			_, err := client.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
			return err
		},
		annotation, false, false,
	)
}

func NodeHasLabel(ctx context.Context, nodeName, label string) (bool, error) {
	kv := strings.SplitN(label, "=", 2)
	key := kv[0]
	val := ""
	if len(kv) == 2 {
		val = kv[1]
	}

	client, err := getK8sClientSet()
	if err != nil {
		return false, err
	}
	node, err := client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	v, ok := node.Labels[key]
	return ok && v == val, nil
}

// NodeHasLabelOrMigrate reads the new label first and migrates a matching legacy
// label in one node update. An explicit new value takes precedence.
func NodeHasLabelOrMigrate(ctx context.Context, nodeName, label, legacyLabel string) (bool, error) {
	client, err := getK8sClientSet()
	if err != nil {
		return false, err
	}
	key, value, _ := strings.Cut(label, "=")
	legacyKey, legacyValue, _ := strings.Cut(legacyLabel, "=")
	node, err := client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	if actual, ok := node.Labels[key]; ok {
		return actual == value, nil
	}
	if actual, ok := node.Labels[legacyKey]; !ok || actual != legacyValue {
		return false, nil
	}
	node.Labels[key] = value
	delete(node.Labels, legacyKey)
	_, err = client.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
	return err == nil, err
}

func LabelPod(ctx context.Context, namespace, name, label string) error {
	client, err := getK8sClientSet()
	if err != nil {
		return err
	}
	return updateObjectMetadata(
		func() (metav1.Object, error) {
			return client.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		},
		func(obj metav1.Object) error {
			pod, ok := obj.(*v1.Pod)
			if !ok {
				return fmt.Errorf("unexpected object type %T, want *v1.Pod", obj)
			}
			_, err := client.CoreV1().Pods(namespace).Update(ctx, pod, metav1.UpdateOptions{})
			return err
		},
		label, true, false,
	)
}

func LabelNode(ctx context.Context, nodeName, label string) error {
	client, err := getK8sClientSet()
	if err != nil {
		return err
	}
	return updateObjectMetadata(
		func() (metav1.Object, error) {
			return client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		},
		func(obj metav1.Object) error {
			node, ok := obj.(*v1.Node)
			if !ok {
				return fmt.Errorf("unexpected object type %T, want *v1.Node", obj)
			}
			_, err := client.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
			return err
		},
		label, true, false,
	)
}

func RemovePodLabel(ctx context.Context, namespace, name, label string) error {
	client, err := getK8sClientSet()
	if err != nil {
		return err
	}
	return updateObjectMetadata(
		func() (metav1.Object, error) {
			return client.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		},
		func(obj metav1.Object) error {
			pod, ok := obj.(*v1.Pod)
			if !ok {
				return fmt.Errorf("unexpected object type %T, want *v1.Pod", obj)
			}
			_, err := client.CoreV1().Pods(namespace).Update(ctx, pod, metav1.UpdateOptions{})
			return err
		},
		label, true, true,
	)
}

func RemoveNodeLabel(ctx context.Context, name, label string) error {
	client, err := getK8sClientSet()
	if err != nil {
		return err
	}
	return updateObjectMetadata(
		func() (metav1.Object, error) {
			return client.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		},
		func(obj metav1.Object) error {
			node, ok := obj.(*v1.Node)
			if !ok {
				return fmt.Errorf("unexpected object type %T, want *v1.Node", obj)
			}
			_, err := client.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
			return err
		},
		label, true, true,
	)
}

func GetConfigMap(ctx context.Context, namespace, name string) (*v1.ConfigMap, error) {
	client, err := getK8sClientSet()
	if err != nil {
		return nil, err
	}
	return client.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
}

func UpdateConfigMap(ctx context.Context, namespace string, configMap *v1.ConfigMap) error {
	client, err := getK8sClientSet()
	if err != nil {
		return err
	}
	_, err = client.CoreV1().ConfigMaps(namespace).Update(ctx, configMap, metav1.UpdateOptions{})
	return err
}

func DeletePod(ctx context.Context, namespace, name string) error {
	client, err := getK8sClientSet()
	if err != nil {
		return err
	}
	return client.CoreV1().Pods(namespace).Delete(ctx, name, metav1.DeleteOptions{})
}

func DeletePodsWithLabel(ctx context.Context, namespace, labelSelector string) error {
	client, err := getK8sClientSet()
	if err != nil {
		return err
	}
	return client.CoreV1().Pods(namespace).DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
}

func GetPodNamesByLabel(ctx context.Context, namespace, labelSelector string) ([]string, error) {
	client, err := getK8sClientSet()
	if err != nil {
		return nil, err
	}
	pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return nil, err
	}
	var names []string
	for _, pod := range pods.Items {
		names = append(names, pod.Name)
	}
	return names, nil
}

func GetNodeNamesByLabel(ctx context.Context, labelSelector string) ([]string, error) {
	client, err := getK8sClientSet()
	if err != nil {
		return nil, err
	}
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return nil, err
	}
	var names []string
	for _, node := range nodes.Items {
		names = append(names, node.Name)
	}
	return names, nil
}

func CordonNode(ctx context.Context, nodeName string) error {
	client, err := getK8sClientSet()
	if err != nil {
		return err
	}
	node, err := client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if !node.Spec.Unschedulable {
		node.Spec.Unschedulable = true
		_, err = client.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
	}
	return nil
}

func DeleteNode(ctx context.Context, nodeName string) error {
	client, err := getK8sClientSet()
	if err != nil {
		return err
	}
	return client.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
}

func GetNodesNamesOnly(ctx context.Context) ([]string, error) {
	client, err := getK8sClientSet()
	if err != nil {
		return nil, err
	}
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var names []string
	for _, node := range nodes.Items {
		names = append(names, node.Name)
	}
	return names, nil
}
