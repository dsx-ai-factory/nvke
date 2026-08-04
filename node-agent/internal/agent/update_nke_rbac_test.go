package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	ktesting "k8s.io/client-go/testing"

	pb "github.com/NVIDIA/nke/node-agent/pkg/agent/proto"
)

const (
	defaultNKERBACName           = "nke:cwe"
	secondaryNKERBACName         = "nke:swe"
	defaultNamespacedNKERBACName = "nke:cwe-rbac"
	sharedNamespacedNKERBACName  = "nke:shared"
	defaultRBACNamespace         = "nke-system"
	secondaryRBACNamespace       = "tenant-system"
)

var allowedClusterRoleManifest = clusterRoleManifest(defaultNKERBACName)

func TestUpdateNKERBACManifestRejected(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		wantErr  string
	}{
		{
			name:     "empty manifest",
			manifest: "",
			wantErr:  "manifest must contain at least one resource",
		},
		{
			name:     "only empty documents",
			manifest: "\n---\n# comment\n---\n",
			wantErr:  "manifest must contain at least one resource",
		},
		{
			name: "malformed YAML",
			manifest: `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: [`,
			wantErr: "parsing Kubernetes object from document 1",
		},
		{
			name: "unsupported apiVersion",
			manifest: `apiVersion: v1
kind: ClusterRole
metadata:
  name: nke:cwe`,
			wantErr: "unsupported apiVersion",
		},
		{
			name: "missing apiVersion",
			manifest: `kind: ClusterRole
metadata:
  name: nke:cwe`,
			wantErr: "apiVersion is required",
		},
		{
			name: "missing kind",
			manifest: `apiVersion: rbac.authorization.k8s.io/v1
metadata:
  name: nke:cwe`,
			wantErr: "kind is required",
		},
		{
			name: "missing metadata name",
			manifest: `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {}`,
			wantErr: "metadata.name is required",
		},
		{
			name: "disallowed kind",
			manifest: `apiVersion: rbac.authorization.k8s.io/v1
kind: ConfigMap
metadata:
  name: nke:cwe`,
			wantErr: "unsupported kind",
		},
		{
			name: "not a single Kubernetes object",
			manifest: `- apiVersion: rbac.authorization.k8s.io/v1
  kind: ClusterRole
  metadata:
    name: nke:cwe`,
			wantErr: "parsing Kubernetes object from document 1",
		},
		{
			name:     "ClusterRole with disallowed name",
			manifest: clusterRoleManifest("other-name"),
			wantErr:  `names must start with "nke:"`,
		},
		{
			name:     "ClusterRole with bare prefix only",
			manifest: clusterRoleManifest(nkeRBACNamePrefix),
			wantErr:  `names must start with "nke:"`,
		},
		{
			name:     "ClusterRole must not set namespace",
			manifest: clusterRoleManifestWithNamespace(defaultNKERBACName, defaultRBACNamespace),
			wantErr:  "must not set metadata.namespace",
		},
		{
			name:     "Role must set namespace",
			manifest: roleManifestNoNamespace(defaultNamespacedNKERBACName),
			wantErr:  "must set metadata.namespace",
		},
		{
			name:     "ClusterRoleBinding roleRef name must use nke prefix",
			manifest: clusterRoleBindingManifest(defaultNKERBACName, rbacKindClusterRole, "other-name"),
			wantErr:  `roleRef.name "other-name" is not allowed`,
		},
		{
			name:     "duplicate ClusterRole",
			manifest: string(joinManifests(clusterRoleManifest(defaultNKERBACName), clusterRoleManifest(defaultNKERBACName))),
			wantErr:  "duplicated Kubernetes object",
		},
		{
			name: "duplicate Role in same namespace",
			manifest: string(joinManifests(
				roleManifest(defaultNamespacedNKERBACName, defaultRBACNamespace),
				roleManifest(defaultNamespacedNKERBACName, defaultRBACNamespace),
			)),
			wantErr: "duplicated Kubernetes object",
		},
		{
			name: "allowed document plus disallowed document",
			manifest: string(joinManifests(
				clusterRoleManifest(defaultNKERBACName),
				roleManifestNoNamespace(defaultNamespacedNKERBACName),
			)),
			wantErr: "validating Kubernetes object from document 2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := newUpdateRBACServerWithClient(newDynamicClient())

			resp, err := server.UpdateNKERBAC(context.Background(), &pb.UpdateNKERBACRequest{
				Manifest: []byte(tt.manifest),
			})

			require.Error(t, err)
			assert.Nil(t, resp)
			assert.Equal(t, codes.InvalidArgument, status.Code(err))
			require.ErrorContains(t, err, "update NKE RBAC: invalid manifest")
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestUpdateNKERBACApply(t *testing.T) {
	const manifestWithManagedFields = `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: nke:cwe
  managedFields:
  - apiVersion: rbac.authorization.k8s.io/v1
    fieldsType: FieldsV1
    manager: kubectl
    operation: Apply
rules:
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["get"]
`

	type want struct {
		name      string
		kind      string
		namespace string
		outcome   pb.ResourceApplyResult_Outcome
	}

	tests := []struct {
		name        string
		manifest    []byte
		setup       func(*dynamicfake.FakeDynamicClient)
		wantResults []want
		thenAssert  func(*testing.T, dynamic.Interface)
	}{
		{
			name:     "ClusterRole CREATED preserves manifest payload",
			manifest: []byte(clusterRoleManifest(defaultNKERBACName)),
			wantResults: []want{
				{name: defaultNKERBACName, kind: rbacKindClusterRole, outcome: pb.ResourceApplyResult_OUTCOME_CREATED},
			},
			thenAssert: func(t *testing.T, c dynamic.Interface) {
				obj := getObject(t, c, rbacKindClusterRole, "", defaultNKERBACName)
				assert.Equal(t, rbacAPIVersion, obj.GetAPIVersion())
				rules, found, err := unstructured.NestedSlice(obj.Object, "rules")
				require.NoError(t, err)
				require.True(t, found, "rules should be carried through to apply")
				assert.NotEmpty(t, rules)
			},
		},
		{
			name:     "ClusterRoleBinding CREATED",
			manifest: []byte(clusterRoleBindingManifest(defaultNKERBACName, rbacKindClusterRole, defaultNKERBACName)),
			wantResults: []want{
				{name: defaultNKERBACName, kind: rbacKindClusterRoleBinding, outcome: pb.ResourceApplyResult_OUTCOME_CREATED},
			},
		},
		{
			name:     "Role CREATED in namespace",
			manifest: []byte(roleManifest(defaultNamespacedNKERBACName, defaultRBACNamespace)),
			wantResults: []want{
				{
					name:      defaultNamespacedNKERBACName,
					kind:      rbacKindRole,
					namespace: defaultRBACNamespace,
					outcome:   pb.ResourceApplyResult_OUTCOME_CREATED,
				},
			},
			thenAssert: func(t *testing.T, c dynamic.Interface) {
				obj := getObject(t, c, rbacKindRole, defaultRBACNamespace, defaultNamespacedNKERBACName)
				assert.Equal(t, defaultRBACNamespace, obj.GetNamespace())
			},
		},
		{
			name:     "RoleBinding to Role CREATED",
			manifest: []byte(roleBindingManifest(defaultNamespacedNKERBACName, defaultRBACNamespace, rbacKindRole, defaultNamespacedNKERBACName)),
			wantResults: []want{
				{
					name:      defaultNamespacedNKERBACName,
					kind:      rbacKindRoleBinding,
					namespace: defaultRBACNamespace,
					outcome:   pb.ResourceApplyResult_OUTCOME_CREATED,
				},
			},
		},
		{
			name:     "RoleBinding to ClusterRole CREATED",
			manifest: []byte(roleBindingManifest(secondaryNKERBACName, defaultRBACNamespace, rbacKindClusterRole, secondaryNKERBACName)),
			wantResults: []want{
				{
					name:      secondaryNKERBACName,
					kind:      rbacKindRoleBinding,
					namespace: defaultRBACNamespace,
					outcome:   pb.ResourceApplyResult_OUTCOME_CREATED,
				},
			},
		},
		{
			name:        "ClusterRole UNCHANGED when generation does not change",
			manifest:    []byte(clusterRoleManifest(defaultNKERBACName)),
			setup:       injectGenerations(rbacKindClusterRole, defaultNKERBACName, "", 12, 12),
			wantResults: []want{{name: defaultNKERBACName, kind: rbacKindClusterRole, outcome: pb.ResourceApplyResult_OUTCOME_UNCHANGED}},
		},
		{
			name:        "ClusterRole UPDATED when generation changes",
			manifest:    []byte(clusterRoleManifest(defaultNKERBACName)),
			setup:       injectGenerations(rbacKindClusterRole, defaultNKERBACName, "", 12, 13),
			wantResults: []want{{name: defaultNKERBACName, kind: rbacKindClusterRole, outcome: pb.ResourceApplyResult_OUTCOME_UPDATED}},
		},
		{
			name:        "ClusterRoleBinding UNCHANGED when generation does not change",
			manifest:    []byte(clusterRoleBindingManifest(defaultNKERBACName, rbacKindClusterRole, defaultNKERBACName)),
			setup:       injectGenerations(rbacKindClusterRoleBinding, defaultNKERBACName, "", 7, 7),
			wantResults: []want{{name: defaultNKERBACName, kind: rbacKindClusterRoleBinding, outcome: pb.ResourceApplyResult_OUTCOME_UNCHANGED}},
		},
		{
			name:        "ClusterRoleBinding UPDATED when generation changes",
			manifest:    []byte(clusterRoleBindingManifest(defaultNKERBACName, rbacKindClusterRole, defaultNKERBACName)),
			setup:       injectGenerations(rbacKindClusterRoleBinding, defaultNKERBACName, "", 7, 8),
			wantResults: []want{{name: defaultNKERBACName, kind: rbacKindClusterRoleBinding, outcome: pb.ResourceApplyResult_OUTCOME_UPDATED}},
		},
		{
			name: "same Role name in different namespaces is allowed and preserves order",
			manifest: joinManifests(
				roleManifest(sharedNamespacedNKERBACName, defaultRBACNamespace),
				roleManifest(sharedNamespacedNKERBACName, secondaryRBACNamespace),
				clusterRoleManifest(secondaryNKERBACName),
			),
			wantResults: []want{
				{
					name:      sharedNamespacedNKERBACName,
					kind:      rbacKindRole,
					namespace: defaultRBACNamespace,
					outcome:   pb.ResourceApplyResult_OUTCOME_CREATED,
				},
				{
					name:      sharedNamespacedNKERBACName,
					kind:      rbacKindRole,
					namespace: secondaryRBACNamespace,
					outcome:   pb.ResourceApplyResult_OUTCOME_CREATED,
				},
				{name: secondaryNKERBACName, kind: rbacKindClusterRole, outcome: pb.ResourceApplyResult_OUTCOME_CREATED},
			},
		},
		{
			name:     "managedFields are stripped before apply",
			manifest: []byte(manifestWithManagedFields),
			wantResults: []want{
				{name: defaultNKERBACName, kind: rbacKindClusterRole, outcome: pb.ResourceApplyResult_OUTCOME_CREATED},
			},
			thenAssert: func(t *testing.T, c dynamic.Interface) {
				obj := getObject(t, c, rbacKindClusterRole, "", defaultNKERBACName)
				_, found, err := unstructured.NestedFieldNoCopy(obj.Object, "metadata", "managedFields")
				require.NoError(t, err)
				assert.False(t, found, "managedFields must be stripped before apply")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client := newDynamicClient()
			if tt.setup != nil {
				tt.setup(client)
			}
			server := newUpdateRBACServerWithClient(client)

			resp, err := server.UpdateNKERBAC(context.Background(), &pb.UpdateNKERBACRequest{
				Manifest: tt.manifest,
			})

			require.NoError(t, err)
			require.Len(t, resp.GetResults(), len(tt.wantResults))
			for i, w := range tt.wantResults {
				got := resp.GetResults()[i]
				assert.Equal(t, w.name, got.GetName(), "result[%d].Name", i)
				assert.Equal(t, w.kind, got.GetKind(), "result[%d].Kind", i)
				assert.Equal(t, w.namespace, got.GetNamespace(), "result[%d].Namespace", i)
				assert.Equal(t, w.outcome, got.GetOutcome(), "result[%d].Outcome", i)
			}
			if tt.thenAssert != nil {
				tt.thenAssert(t, client)
			}
		})
	}
}

func TestUpdateNKERBACApplyErrors(t *testing.T) {
	t.Run("Get error returns FAILED without patching", func(t *testing.T) {
		t.Parallel()
		client := newDynamicClient()
		patchCalls := 0
		client.PrependReactor("get", "clusterroles", func(ktesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("get clusterrole failed")
		})
		client.PrependReactor("patch", "clusterroles", func(ktesting.Action) (bool, runtime.Object, error) {
			patchCalls++
			return false, nil, nil
		})
		server := newUpdateRBACServerWithClient(client)

		resp, err := server.UpdateNKERBAC(context.Background(), &pb.UpdateNKERBACRequest{
			Manifest: []byte(clusterRoleManifest(defaultNKERBACName)),
		})

		require.NoError(t, err)
		require.Len(t, resp.GetResults(), 1)
		got := resp.GetResults()[0]
		assert.Equal(t, rbacKindClusterRole, got.GetKind())
		assert.Equal(t, defaultNKERBACName, got.GetName())
		assert.Empty(t, got.GetNamespace())
		assert.Equal(t, pb.ResourceApplyResult_OUTCOME_FAILED, got.GetOutcome())
		assert.Contains(t, got.GetErrorMessage(), "reading")
		assert.Contains(t, got.GetErrorMessage(), "before apply")
		assert.Contains(t, got.GetErrorMessage(), "get clusterrole failed")
		assert.Zero(t, patchCalls, "Patch should not be called when Get fails")
	})

	t.Run("Apply error returns FAILED after a previous CREATED", func(t *testing.T) {
		t.Parallel()
		client := newDynamicClient()
		bindingPatchCalls := 0
		client.PrependReactor("patch", "clusterrolebindings", func(action ktesting.Action) (bool, runtime.Object, error) {
			patchAction, ok := action.(ktesting.PatchAction)
			if !ok || patchAction.GetPatchType() != types.ApplyPatchType {
				return false, nil, nil
			}
			bindingPatchCalls++
			return true, nil, errors.New("apply binding failed")
		})
		server := newUpdateRBACServerWithClient(client)

		resp, err := server.UpdateNKERBAC(context.Background(), &pb.UpdateNKERBACRequest{
			Manifest: joinManifests(
				clusterRoleManifest(defaultNKERBACName),
				clusterRoleBindingManifest(defaultNKERBACName, rbacKindClusterRole, defaultNKERBACName),
			),
		})

		require.NoError(t, err)
		require.Len(t, resp.GetResults(), 2)
		first := resp.GetResults()[0]
		assert.Equal(t, rbacKindClusterRole, first.GetKind())
		assert.Equal(t, defaultNKERBACName, first.GetName())
		assert.Empty(t, first.GetNamespace())
		assert.Equal(t, pb.ResourceApplyResult_OUTCOME_CREATED, first.GetOutcome())
		second := resp.GetResults()[1]
		assert.Equal(t, rbacKindClusterRoleBinding, second.GetKind())
		assert.Equal(t, defaultNKERBACName, second.GetName())
		assert.Empty(t, second.GetNamespace())
		assert.Equal(t, pb.ResourceApplyResult_OUTCOME_FAILED, second.GetOutcome())
		assert.Contains(t, second.GetErrorMessage(), "applying")
		assert.Contains(t, second.GetErrorMessage(), "apply binding failed")
		assert.Equal(t, 1, bindingPatchCalls, "binding apply should be called once and not retried")
	})
}

type fakeClientRegistry struct {
	client             kubernetes.Interface
	dynamicClient      dynamic.Interface
	localClient        *kubernetes.Clientset
	err                error
	clientCalls        int
	dynamicClientCalls int
	localClientCalls   int
}

func (r *fakeClientRegistry) Client() (kubernetes.Interface, error) {
	r.clientCalls++
	return r.client, r.err
}

func (r *fakeClientRegistry) DynamicClient() (dynamic.Interface, error) {
	r.dynamicClientCalls++
	return r.dynamicClient, r.err
}

func (r *fakeClientRegistry) LocalClient() (*kubernetes.Clientset, error) {
	r.localClientCalls++
	return r.localClient, r.err
}

// newDynamicClient builds a fake dynamic.Interface that emulates the apply path
// of a real apiserver against an in-memory store. Get returns a stored object or
// NotFound, and ApplyPatchType patches deserialize the payload, stash it in the
// store, and echo it back. Tests can override individual reactions via
// PrependReactor on the returned client.
func newDynamicClient(objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), objects...)
	store := map[schema.GroupVersionResource]map[string]*unstructured.Unstructured{}

	for _, obj := range objects {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		kindSpec, ok := rbacKindSpecs[u.GetKind()]
		if !ok {
			continue
		}
		storeRBACObject(store, kindSpec.gvr, u)
	}

	client.PrependReactor("get", "*", func(action ktesting.Action) (bool, runtime.Object, error) {
		getAction, ok := action.(ktesting.GetAction)
		if !ok {
			return false, nil, nil
		}
		obj, ok := getRBACObject(store, getAction.GetResource(), getAction.GetNamespace(), getAction.GetName())
		if !ok {
			return true, nil, apierrors.NewNotFound(getAction.GetResource().GroupResource(), getAction.GetName())
		}
		return true, obj, nil
	})

	client.PrependReactor("patch", "*", func(action ktesting.Action) (bool, runtime.Object, error) {
		patchAction, ok := action.(ktesting.PatchAction)
		if !ok || patchAction.GetPatchType() != types.ApplyPatchType {
			return false, nil, nil
		}

		obj := &unstructured.Unstructured{}
		if err := json.Unmarshal(patchAction.GetPatch(), &obj.Object); err != nil {
			return true, nil, err
		}
		obj.SetName(patchAction.GetName())
		obj.SetNamespace(patchAction.GetNamespace())
		storeRBACObject(store, patchAction.GetResource(), obj)
		return true, obj.DeepCopy(), nil
	})

	return client
}

func rbacStoreKey(namespace, name string) string {
	if namespace == "" {
		return name
	}
	return namespace + "/" + name
}

func storeRBACObject(
	store map[schema.GroupVersionResource]map[string]*unstructured.Unstructured,
	gvr schema.GroupVersionResource,
	obj *unstructured.Unstructured,
) {
	if store[gvr] == nil {
		store[gvr] = map[string]*unstructured.Unstructured{}
	}
	store[gvr][rbacStoreKey(obj.GetNamespace(), obj.GetName())] = obj.DeepCopy()
}

func getRBACObject(
	store map[schema.GroupVersionResource]map[string]*unstructured.Unstructured,
	gvr schema.GroupVersionResource,
	namespace string,
	name string,
) (*unstructured.Unstructured, bool) {
	obj, ok := store[gvr][rbacStoreKey(namespace, name)]
	if !ok {
		return nil, false
	}
	return obj.DeepCopy(), true
}

func newRBACResource(kind, name, namespace string, generation int64) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion(rbacAPIVersion)
	obj.SetKind(kind)
	obj.SetName(name)
	obj.SetNamespace(namespace)
	obj.SetGeneration(generation)
	return obj
}

// getObject retrieves the named RBAC resource of the given kind from the
// fake client. newDynamicClient's apply reactor stores the patch payload
// verbatim, so this returns the object the agent actually sent.
func getObject(t *testing.T, client dynamic.Interface, kind, namespace, name string) *unstructured.Unstructured {
	t.Helper()
	kindSpec, ok := rbacKindSpecs[kind]
	require.Truef(t, ok, "unexpected RBAC kind %q", kind)
	var resourceClient dynamic.ResourceInterface
	if kindSpec.namespaced {
		resourceClient = client.Resource(kindSpec.gvr).Namespace(namespace)
	} else {
		resourceClient = client.Resource(kindSpec.gvr)
	}
	obj, err := resourceClient.Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(t, err)
	return obj
}

// injectGenerations configures the fake client so that Get reports an existing
// resource at beforeGen and Apply returns one at afterGen. This drives the
// UNCHANGED/UPDATED outcome classification in applyNKERBACResource.
func injectGenerations(
	kind string,
	name string,
	namespace string,
	beforeGen int64,
	afterGen int64,
) func(*dynamicfake.FakeDynamicClient) {
	kindSpec, ok := rbacKindSpecs[kind]
	if !ok {
		panic(fmt.Sprintf("unexpected RBAC kind %q", kind))
	}
	resource := kindSpec.gvr.Resource
	return func(c *dynamicfake.FakeDynamicClient) {
		c.PrependReactor("get", resource, func(action ktesting.Action) (bool, runtime.Object, error) {
			getAction, ok := action.(ktesting.GetAction)
			if !ok {
				return false, nil, nil
			}
			if getAction.GetName() != name || getAction.GetNamespace() != namespace {
				return false, nil, nil
			}
			return true, newRBACResource(kind, name, namespace, beforeGen), nil
		})
		c.PrependReactor("patch", resource, func(action ktesting.Action) (bool, runtime.Object, error) {
			patchAction, ok := action.(ktesting.PatchAction)
			if !ok || patchAction.GetName() != name || patchAction.GetNamespace() != namespace {
				return false, nil, nil
			}
			return true, newRBACResource(kind, name, namespace, afterGen), nil
		})
	}
}

func newUpdateRBACServer(registry kubernetesClientRegistry) *NodeAgentServer {
	return &NodeAgentServer{
		NodeStatus:     &NodeStatus{Role: ControlPlaneRole},
		clientRegistry: registry,
	}
}

func newUpdateRBACServerWithClient(client dynamic.Interface) *NodeAgentServer {
	return newUpdateRBACServer(&fakeClientRegistry{dynamicClient: client})
}

func joinManifests(docs ...string) []byte {
	return []byte(strings.Join(docs, "\n---\n"))
}

func clusterRoleManifest(name string) string {
	return fmt.Sprintf(`apiVersion: %s
kind: %s
metadata:
  name: %q
rules:
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["get"]
`, rbacAPIVersion, rbacKindClusterRole, name)
}

func clusterRoleManifestWithNamespace(name, namespace string) string {
	return fmt.Sprintf(`apiVersion: %s
kind: %s
metadata:
  name: %q
  namespace: %q
rules:
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["get"]
`, rbacAPIVersion, rbacKindClusterRole, name, namespace)
}

func clusterRoleBindingManifest(name, roleRefKind, roleRefName string) string {
	return clusterRoleBindingManifestWithAPIGroup(name, roleRefKind, roleRefName, rbacAPIGroup)
}

func clusterRoleBindingManifestWithAPIGroup(name, roleRefKind, roleRefName, roleRefAPIGroup string) string {
	return fmt.Sprintf(`apiVersion: %s
kind: %s
metadata:
  name: %q
roleRef:
  apiGroup: %q
  kind: %q
  name: %q
subjects:
- kind: ServiceAccount
  name: cwe
  namespace: kube-system
`, rbacAPIVersion, rbacKindClusterRoleBinding, name, roleRefAPIGroup, roleRefKind, roleRefName)
}

func roleManifest(name, namespace string) string {
	return fmt.Sprintf(`apiVersion: %s
kind: %s
metadata:
  name: %q
  namespace: %q
rules:
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["get"]
`, rbacAPIVersion, rbacKindRole, name, namespace)
}

func roleManifestNoNamespace(name string) string {
	return fmt.Sprintf(`apiVersion: %s
kind: %s
metadata:
  name: %q
rules:
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["get"]
`, rbacAPIVersion, rbacKindRole, name)
}

func roleBindingManifest(name, namespace, roleRefKind, roleRefName string) string {
	return roleBindingManifestWithAPIGroup(name, namespace, roleRefKind, roleRefName, rbacAPIGroup)
}

func roleBindingManifestWithAPIGroup(
	name string,
	namespace string,
	roleRefKind string,
	roleRefName string,
	roleRefAPIGroup string,
) string {
	return fmt.Sprintf(`apiVersion: %s
kind: %s
metadata:
  name: %q
  namespace: %q
roleRef:
  apiGroup: %q
  kind: %q
  name: %q
subjects:
- kind: ServiceAccount
  name: cwe
  namespace: %q
`, rbacAPIVersion, rbacKindRoleBinding, name, namespace, roleRefAPIGroup, roleRefKind, roleRefName, namespace)
}
