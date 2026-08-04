package k8s

import (
	"context"
	"os"
	"strings"
	"sync"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	clientset         kubernetes.Interface
	clientsetOnce     sync.Once
	clientsetErr      error
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
			clientsetErr = err
			return
		}
		config, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
		if err != nil {
			clientsetErr = err
			return
		}
		clientset, clientsetErr = kubernetes.NewForConfig(config)
	})
	return clientset, clientsetErr
}

func updateObjectMetadata(getObj func() (metav1.Object, error), updateObj func(metav1.Object) error, kvStr string, isLabel bool, remove bool) error {
	kv := strings.SplitN(kvStr, "=", 2)
	if len(kv) < 1 {
		return nil
	}
	key := kv[0]
	val := ""
	if len(kv) == 2 {
		val = kv[1]
	}

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
			pod := obj.(*v1.Pod)
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
			node := obj.(*v1.Node)
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
			pod := obj.(*v1.Pod)
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
			node := obj.(*v1.Node)
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
			pod := obj.(*v1.Pod)
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
			node := obj.(*v1.Node)
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
