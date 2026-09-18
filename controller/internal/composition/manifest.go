// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package composition

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

const (
	// manifestDir holds vendored upstream manifests and the pins that describe
	// them. Vendored rather than fetched so generation needs no network and the
	// bytes are reviewable in the pull request that bumps them; CAPI core is the
	// one that cannot be, at ~25k lines carrying placeholders clusterctl
	// resolves.
	manifestDir = "manifests"
	pinSuffix   = ".pin.yaml"

	defaultClassAnnotation = "storageclass.kubernetes.io/is-default-class"
)

// ManifestPin describes one vendored upstream manifest and the edits applied to
// it when the item is generated.
//
// Patching at generate time rather than after apply is what makes the item a
// plain kubectl apply with nothing to remember: the exercise had to patch the
// Deployment and the StorageClass afterwards, and the second left a window in
// which a PersistentVolumeClaim could bind to nothing.
type ManifestPin struct {
	// Item is the deploy-path item this becomes, and therefore its directory.
	Item string `yaml:"item"`
	// Version is the upstream release this was taken from.
	Version string `yaml:"version"`
	// File is the vendored manifest, relative to manifests/.
	File string `yaml:"file"`
	// SHA256 is the upstream asset's checksum, verified on every generate so an
	// edited vendored file fails here rather than on a cluster.
	SHA256 string        `yaml:"sha256"`
	Patch  ManifestPatch `yaml:"patch"`
}

// ManifestPatch is the set of edits a pin applies. Every field is optional.
type ManifestPatch struct {
	// Args replaces the container's argument list wholesale, so an argument the
	// upstream manifest already carries has to be restated or it is lost.
	Args []string `yaml:"args,omitempty"`
	// ImagePullSecrets is added to the pod spec for a manifest that carries
	// none, naming a Secret an earlier item creates.
	ImagePullSecrets []PullSecretRef `yaml:"imagePullSecrets,omitempty"`
	// DefaultStorageClass names the StorageClass to annotate as the cluster
	// default.
	DefaultStorageClass string `yaml:"defaultStorageClass,omitempty"`
}

type PullSecretRef struct {
	Name string `yaml:"name"`
}

// emitManifestItems renders the vendored manifests a target selects, in the
// order it lists them.
func (r *Recipe) emitManifestItems(target Target) (map[string][]byte, []item, error) {
	files := map[string][]byte{}
	var items []item

	for _, name := range target.Manifests {
		pin, err := r.loadManifestPin(name)
		if err != nil {
			return nil, nil, err
		}
		content, err := r.renderManifest(name, pin)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", name, err)
		}
		files[filepath.Join(pin.Item, pin.File)] = content
		items = append(items, item{Name: pin.Item, Type: TypeManifest})
	}
	return files, items, nil
}

// loadManifestPin reads manifests/<name>.pin.yaml.
func (r *Recipe) loadManifestPin(name string) (ManifestPin, error) {
	var pin ManifestPin
	path := filepath.Join(r.Dir, manifestDir, name+pinSuffix)
	if err := readYAML(path, &pin); err != nil {
		return pin, err
	}
	switch {
	case pin.Item == "":
		return pin, fmt.Errorf("%s: item is required", path)
	case pin.File == "":
		return pin, fmt.Errorf("%s: file is required", path)
	case pin.SHA256 == "":
		return pin, fmt.Errorf("%s: sha256 is required", path)
	}
	return pin, nil
}

// renderManifest reads the vendored manifest, verifies it against the pin, and
// applies the pin's edits. ref is the recipe's name for the pin, which is what
// loadManifestPin resolved the file from -- the pin is named for the item, not
// for the version-carrying manifest beside it.
func (r *Recipe) renderManifest(ref string, pin ManifestPin) ([]byte, error) {
	raw, err := os.ReadFile(filepath.Join(r.Dir, manifestDir, pin.File))
	if err != nil {
		return nil, err
	}
	if sum := hex.EncodeToString(sha256Sum(raw)); sum != pin.SHA256 {
		return nil, fmt.Errorf("%s does not match the pin: got %s, want %s\n"+
			"the vendored file was edited, or the pin was bumped without replacing it",
			pin.File, sum, pin.SHA256)
	}

	docs, err := decodeAll(raw)
	if err != nil {
		return nil, err
	}
	for _, doc := range docs {
		applyPatch(doc, pin.Patch)
	}

	// A pin with no version is a copy of one of this repository's own files.
	// Claiming it was "vendored from its upstream release" would be false
	// provenance in a generated artifact, which is the one thing this line exists
	// to get right.
	provenance := fmt.Sprintf("%s %s, vendored from its upstream release and patched per\n# manifests/%s%s",
		pin.Item, pin.Version, ref, pinSuffix)
	if pin.Version == "" {
		provenance = fmt.Sprintf("%s, copied from this repository per manifests/%s%s", pin.Item, ref, pinSuffix)
	}

	var b bytes.Buffer
	fmt.Fprintf(&b, `# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# GENERATED by %s from controller/deploy/composition. Do not edit.
#
# %s. Applied with kubectl apply --server-side.
`, generatedBy, provenance)

	for _, doc := range docs {
		out, err := yaml.Marshal(doc)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(&b, "---\n%s", out)
	}
	return b.Bytes(), nil
}

// applyPatch edits one decoded document in place, if it is one the pin names.
func applyPatch(doc map[string]any, patch ManifestPatch) {
	switch doc["kind"] {
	case "Deployment":
		podSpec := dig(doc, "spec", "template", "spec")
		if podSpec == nil {
			return
		}
		if len(patch.ImagePullSecrets) > 0 {
			secrets := make([]any, 0, len(patch.ImagePullSecrets))
			for _, ref := range patch.ImagePullSecrets {
				secrets = append(secrets, map[string]any{"name": ref.Name})
			}
			podSpec["imagePullSecrets"] = secrets
		}
		if len(patch.Args) > 0 {
			containers, _ := podSpec["containers"].([]any)
			for _, entry := range containers {
				container, _ := entry.(map[string]any)
				if container == nil {
					continue
				}
				args := make([]any, 0, len(patch.Args))
				for _, arg := range patch.Args {
					args = append(args, arg)
				}
				container["args"] = args
			}
		}

	case "StorageClass":
		if patch.DefaultStorageClass == "" || name(doc) != patch.DefaultStorageClass {
			return
		}
		metadata := dig(doc, "metadata")
		if metadata == nil {
			return
		}
		annotations, _ := metadata["annotations"].(map[string]any)
		if annotations == nil {
			annotations = map[string]any{}
			metadata["annotations"] = annotations
		}
		// Quoted: Kubernetes reads this annotation as a string, and an unquoted
		// true would marshal as a boolean the API server rejects.
		annotations[defaultClassAnnotation] = "true"
	}
}

// dig walks nested maps, returning nil at the first step that is not one.
func dig(node map[string]any, path ...string) map[string]any {
	for _, key := range path {
		next, ok := node[key].(map[string]any)
		if !ok {
			return nil
		}
		node = next
	}
	return node
}

// name returns a document's metadata.name.
func name(doc map[string]any) string {
	metadata := dig(doc, "metadata")
	if metadata == nil {
		return ""
	}
	value, _ := metadata["name"].(string)
	return value
}

// decodeAll decodes every document of a multi-document manifest.
func decodeAll(raw []byte) ([]map[string]any, error) {
	var docs []map[string]any
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			return nil, fmt.Errorf("decoding the vendored manifest: %w", err)
		}
		// A trailing separator decodes to nothing.
		if len(doc) == 0 {
			continue
		}
		docs = append(docs, doc)
	}
	return docs, nil
}

func sha256Sum(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}
