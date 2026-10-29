// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package deploypath

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"

	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/controllerdeploy"
	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/sitespec"
	"github.com/dsx-ai-factory/nvke/cli/internal/deploypath/stackconfig"
)

// SiteValuesFile is the values file render appends to an item for the site.
const SiteValuesFile = "site-values.yaml"

// oidcNodePort is the NodePort the SPIRE OIDC discovery provider is published
// on, by the spire-oidc-provider-nodeport item.
const oidcNodePort = 30443

// controllerAdminPath is the SPIFFE path the controller presents:
// /nvke-service/<ServiceAccount>, and the deploy path names the ServiceAccount
// nvke-controller. It must match spire-server.adminIDs in spire/values.yaml.
const controllerAdminPath = "/nvke-service/nvke-controller"

// temporalImages are the repositories the Temporal chart defaults to; the
// chart has no registry key, so a mirror is written into the repository.
var temporalImages = map[string]string{"server": "temporalio/server", "admintools": "temporalio/admin-tools", "web": "temporalio/ui"}

// TemplateContext is what a template item's .yaml.tmpl files render against:
// the subset of the BootstrapConfig fields in
// docs/site-bootstrap/deploy-repository.md that the site spec, the stack
// config and the environment supply. A template naming another field is
// refused.
type TemplateContext struct {
	ClusterName string
	Nico        struct{ Endpoint, OrgID, Token, APIName string }
	Tenant      struct{ User, Password string }
}

// site writes the site values and renders the template items.
func (r *renderer) site() {
	spec, cs := r.opts.Site, r.opts.Stack
	// An external render reads no secret: no credential variable, no CA file.
	external := r.opts.Credentials == CredentialsExternal
	var creds stackconfig.Credentials
	if !external {
		loaded, err := stackconfig.CredentialsFromEnv()
		if err != nil {
			for _, line := range strings.Split(err.Error(), "\n") {
				r.problem("environment: %s", line)
			}
			return
		}
		creds = loaded
	}
	vip, err := spec.ControllerEndpoint()
	if err != nil {
		r.problem("site spec: %v", err)
		return
	}
	// Every template value lands inside a double-quoted YAML scalar or a JSON
	// document, which carry no escaping, so a value holding a quote, a
	// backslash or a control character is refused rather than written
	// altered. An empty value is refused too: it would render an empty
	// credential.
	check := func(what, value string) string {
		if value == "" {
			r.problem("stack config: %s is empty", what)
			return ""
		}
		if !stackconfig.TemplateSafe(value) {
			r.problem("stack config: %s: the value holds a character the templates cannot carry (a quote, a backslash or a control character)", what)
			return ""
		}
		return value
	}
	var ctx TemplateContext
	ctx.ClusterName = spec.ClusterName
	ctx.Nico.Endpoint = check("nico.endpoint", cs.Nico.Endpoint)
	ctx.Nico.OrgID = check("nico.orgID", cs.Nico.OrgID)
	ctx.Nico.APIName = check("nico.apiName", cs.Nico.APIName)
	// CredentialsFromEnv applies the same rule to the credentials.
	ctx.Nico.Token, ctx.Tenant.User, ctx.Tenant.Password = creds.NicoToken, creds.NodeUser, creds.NodePassword
	if len(r.problems) > 0 {
		return
	}
	// The stack joins the trust domain the nodes were bootstrapped with. The
	// default is the one a bootstrap with the default cluster name derives;
	// a bootstrap that was given another name needs spire.trustDomain set.
	// The warning is written now, not noted: it matters even when the render
	// fails later.
	trustDomain, defaulted := spec.TrustDomain()
	if defaulted {
		fmt.Fprintf(r.opts.Log, "warning: spire.trustDomain is not set; using %s, the domain a bootstrap with the default cluster name used. A bootstrap run under another cluster name printed a different one; the install has to use that.\n", trustDomain)
	}
	checksum := controllerdeploy.SpireTPMDirectChecksum(archOf(r.opts.Arch))
	if checksum == "" {
		r.problem("the architecture %q has no TPM attestor checksum", r.opts.Arch)
		return
	}
	password := creds.DatabasePassword

	// controller: the database Secret's name, the OIDC discovery URL and the
	// SPIRE server, all VIP-derived; the images only when the stack config
	// overrides them. No values file carries a credential.
	controller := map[string]any{
		"secrets":      map[string]any{"dbSecretName": databaseSecret},
		"nvkeOIDCAuth": map[string]any{"discoveryURL": fmt.Sprintf("https://%s:%d/.well-known/openid-configuration", vip, oidcNodePort)},
		"spiffe":       map[string]any{"trustDomain": trustDomain, "spire": map[string]any{"serverAddress": vip}},
	}
	if cs.Images.Controller != "" {
		controller["image"] = r.imageValues("images.controller", cs.Images.Controller)
		if r.opts.Release != nil {
			r.note("controller: images.controller in the stack config overrides the release's image")
		}
	}
	if cs.Images.Profiles != "" {
		controller["profiles"] = map[string]any{"image": r.imageValues("images.profiles", cs.Images.Profiles)}
		if r.opts.Release != nil {
			r.note("controller: images.profiles in the stack config overrides the release's profiles image")
		}
	} else if r.opts.Release == nil && r.profilesImageRequired() {
		// With a release, the controller item's profilesArtifact names it.
		r.problem("stack config: images.profiles is required: the deploy path enables profiles.image and nothing else names the image")
	}
	r.siteValues("controller", controller)

	// spire: site identity, the controller's admin ID, the TPM attestor
	// checksum for the architecture, the bootstrap CA when the environment
	// names one, and the OIDC provider published on the VIP.
	global := map[string]any{"trustDomain": trustDomain, "clusterName": spec.ClusterName}
	if cs.Mirror != "" {
		global["image"] = map[string]any{"registry": cs.Mirror}
	}
	server := map[string]any{
		"adminIDs":     []string{"spiffe://" + trustDomain + controllerAdminPath},
		"nodeAttestor": map[string]any{"tpmDirect": map[string]any{"checksum": checksum}},
	}
	// The CA is a Secret the site-credentials item holds; the chart creates none.
	var caCert, caKey []byte
	hasCA := spec.Spire.UpstreamCA
	if hasCA {
		if !external {
			caCert = secretFile(r, sitespec.EnvSpireCACertFile, spec.Spire.CACertFile)
			caKey = secretFile(r, sitespec.EnvSpireCAKeyFile, spec.Spire.CAKeyFile)
		}
		// data.bundle is a marker: the chart sets bundle_file_path only when
		// it is non-empty, and with create: false writes no Secret from it.
		server["upstreamAuthority"] = map[string]any{"disk": map[string]any{
			"enabled": true,
			"secret":  map[string]any{"create": false, "name": spireCASecret, "data": map[string]any{"bundle": "bundle.crt"}},
		}}
	}
	domains, err := oidcDomains(filepath.Join(r.opts.DeployPath, "spire", "overlay.yaml"), vip)
	if err != nil {
		r.problem("spire: %v", err)
		return
	}
	spire := map[string]any{
		"global":       map[string]any{"spire": global},
		"spire-server": server,
		"spiffe-oidc-discovery-provider": map[string]any{
			"config": map[string]any{"jwksUri": fmt.Sprintf("https://%s:%d/keys", vip, oidcNodePort), "additionalDomains": domains},
			"tls":    map[string]any{"certManager": map[string]any{"certificate": map[string]any{"ipAddresses": []string{vip}}}},
		},
	}
	r.siteValues("spire", spire)

	// postgresql and temporal read the one password from the same Secret.
	postgresql := map[string]any{"auth": map[string]any{"existingSecret": databaseSecret}}
	if cs.Mirror != "" {
		// The chart refuses an image outside its own registry unless told so.
		postgresql["global"] = map[string]any{"imageRegistry": cs.Mirror, "security": map[string]any{"allowInsecureImages": true}}
	}
	r.siteValues("postgresql", postgresql)
	temporal := map[string]any{"server": map[string]any{"config": map[string]any{"persistence": map[string]any{
		"default":    map[string]any{"sql": map[string]any{"existingSecret": databaseSecret}},
		"visibility": map[string]any{"sql": map[string]any{"existingSecret": databaseSecret}},
	}}}}
	if cs.Mirror != "" {
		for key, repo := range temporalImages {
			image := map[string]any{"repository": cs.Mirror + "/" + repo}
			if existing, ok := temporal[key].(map[string]any); ok {
				existing["image"] = image
			} else {
				temporal[key] = map[string]any{"image": image}
			}
		}
		r.note("mirror: spire, postgresql and temporal pull from %s; nktp and capnico keep their upstream registries", cs.Mirror)
	}
	r.siteValues("temporal", temporal)
	if len(r.problems) == 0 {
		r.siteCredentials(password, hasCA, caCert, caKey)
	}

	for _, item := range r.items {
		if item.Type == TypeTemplate {
			r.template(item.Name, &ctx)
		}
	}
	env := fmt.Sprintf("SITE_NAME=%s\nSITE_VIP=%s\nSITE_TRUST_DOMAIN=%s\n", spec.ClusterName, vip, trustDomain)
	if err := writePrivate(filepath.Join(r.opts.Out, "site.env"), []byte(env)); err != nil {
		r.problem("%v", err)
	}
}

// siteValues appends item's site values when the deploy path has the item.
func (r *renderer) siteValues(item string, values map[string]any) {
	if !r.hasItem(item, TypeHelm) {
		r.problem("%s: the deploy path has no %s helm item; its site values have nowhere to go", item, item)
		return
	}
	var b bytes.Buffer
	b.WriteString("# Written by nvkectl render deploy-path from the site spec.\n")
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(values); err != nil {
		r.problem("%s: %v", item, err)
		return
	}
	r.appendValues(item, SiteValuesFile, b.Bytes())
	r.note("%s: site values written", item)
}

// template renders an item's .yaml.tmpl files against ctx into the rendered
// item, removes the templates there, and turns the item into a manifest.
func (r *renderer) template(item string, ctx *TemplateContext) {
	src := filepath.Join(r.opts.DeployPath, item)
	dst := filepath.Join(r.opts.Out, item)
	entries, err := os.ReadDir(src)
	if err != nil {
		r.problem("%s: %v", item, err)
		return
	}
	count := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".yaml.tmpl") {
			continue
		}
		text, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			r.problem("%s: %v", item, err)
			return
		}
		// A template that names a field the context does not have fails to
		// execute, so a typo is a refused render.
		tmpl, err := template.New(name).Option("missingkey=error").Parse(string(text))
		if err != nil {
			r.problem("%s/%s: %v", item, name, err)
			return
		}
		var out bytes.Buffer
		if err := tmpl.Execute(&out, ctx); err != nil {
			r.problem("%s/%s: %v", item, name, err)
			return
		}
		rendered := strings.TrimSuffix(name, ".tmpl")
		if err := os.Remove(filepath.Join(dst, name)); err != nil {
			r.problem("%s: %v", item, err)
			return
		}
		count++
		// Each document is classified by its own kind.
		docs, err := yamlDocuments(out.Bytes())
		if err != nil {
			r.problem("%s/%s: %v", item, rendered, err)
			return
		}
		var kept []*yaml.Node
		hasSecret := false
		for _, doc := range docs {
			meta, err := decodeSecretMeta(doc)
			if err != nil {
				r.problem("%s/%s: %v", item, rendered, err)
				return
			}
			if meta.Kind != "Secret" {
				kept = append(kept, doc)
				continue
			}
			hasSecret = true
			if r.opts.Credentials == CredentialsExternal {
				// The operator's secret store delivers it.
				r.expected(meta)
			}
		}
		// Rendered: the file as written. External: the non-Secret documents.
		content := out.Bytes()
		if r.opts.Credentials == CredentialsExternal {
			if len(kept) == 0 {
				continue
			}
			if content, err = encodeDocuments(kept); err != nil {
				r.problem("%s/%s: %v", item, rendered, err)
				return
			}
		}
		if err := writePrivate(filepath.Join(dst, rendered), content); err != nil {
			r.problem("%s: %v", item, err)
			return
		}
		if hasSecret && r.opts.Credentials != CredentialsExternal {
			r.credential(filepath.Join(item, rendered))
		}
	}
	if count == 0 {
		r.problem("%s: a template item with no .yaml.tmpl file", item)
		return
	}
	for i := range r.items {
		if r.items[i].Name == item {
			r.items[i].Type = TypeManifest
			r.itemsChanged = true
		}
	}
	r.note("%s: %d templates rendered; applies as a manifest item", item, count)
}

// imageValues splits an image reference into the chart's image values:
// repository, tag and digest, the unused pin cleared so nothing from the
// overlay survives beside the operator's. A tag beside a digest is dropped;
// the controller chart fills in its appVersion as a placeholder tag and the
// digest pins the pull. A reference with neither tag nor digest is refused.
func (r *renderer) imageValues(what, ref string) map[string]any {
	values := imageValues(ref)
	if values == nil {
		r.problem("stack config: %s: %q names neither a tag nor a digest", what, ref)
		return map[string]any{}
	}
	return values
}

func imageValues(ref string) map[string]any {
	if repo, digest, ok := strings.Cut(ref, "@"); ok {
		slash := strings.LastIndex(repo, "/")
		if colon := strings.LastIndex(repo, ":"); colon > slash {
			repo = repo[:colon]
		}
		return map[string]any{"repository": repo, "digest": digest, "tag": ""}
	}
	slash := strings.LastIndex(ref, "/")
	if colon := strings.LastIndex(ref, ":"); colon > slash {
		return map[string]any{"repository": ref[:colon], "tag": ref[colon+1:], "digest": ""}
	}
	return nil
}

// profilesImageRequired reports whether the deploy path's controller values
// enable profiles.image without naming a tag or digest, so the stack config
// must supply the image. The files are read as nodes: helm accepts a
// repeated key, so the check does too.
func (r *renderer) profilesImageRequired() bool {
	rel, err := ReadRelease(filepath.Join(r.opts.DeployPath, "controller"))
	if err != nil || rel.Chart == nil {
		return false
	}
	var enabled, pinned bool
	for _, file := range rel.Values {
		data, err := os.ReadFile(filepath.Join(r.opts.DeployPath, "controller", file))
		if err != nil {
			continue
		}
		var doc yaml.Node
		if yaml.Unmarshal(data, &doc) != nil || len(doc.Content) == 0 {
			continue
		}
		image := yamlChild(doc.Content[0], "profiles", "image")
		if image == nil {
			continue
		}
		if n := yamlChild(image, "enabled"); n != nil {
			enabled = n.Value == "true"
		}
		if n := yamlChild(image, "tag"); n != nil && n.Value != "" {
			pinned = true
		}
		if n := yamlChild(image, "digest"); n != nil && n.Value != "" {
			pinned = true
		}
	}
	return enabled && !pinned
}

// yamlChild walks mapping nodes by key; the last of a repeated key wins, as
// it does for helm.
func yamlChild(node *yaml.Node, keys ...string) *yaml.Node {
	for _, key := range keys {
		if node == nil || node.Kind != yaml.MappingNode {
			return nil
		}
		var found *yaml.Node
		for i := 0; i+1 < len(node.Content); i += 2 {
			if node.Content[i].Value == key {
				found = node.Content[i+1]
			}
		}
		node = found
	}
	return node
}

// oidcDomains returns the OIDC provider's additionalDomains from the deploy
// path's spire overlay plus the VIP. A values file replaces a list rather
// than appending to it, so the committed hostnames are read, not retyped.
func oidcDomains(overlay, vip string) ([]string, error) {
	data, err := os.ReadFile(overlay)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Provider struct {
			Config struct {
				AdditionalDomains []string `yaml:"additionalDomains"`
			} `yaml:"config"`
		} `yaml:"spiffe-oidc-discovery-provider"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", overlay, err)
	}
	domains := doc.Provider.Config.AdditionalDomains
	if len(domains) == 0 {
		return nil, fmt.Errorf("%s has no spiffe-oidc-discovery-provider.config.additionalDomains", overlay)
	}
	if !contains(domains, vip) {
		domains = append(domains, vip)
	}
	return domains, nil
}

// secretFile reads the credential file a variable names; missing or empty
// is a problem naming the variable.
func secretFile(r *renderer, what, path string) []byte {
	data, err := os.ReadFile(path)
	if err != nil {
		r.problem("environment: %s: %v", what, err)
		return nil
	}
	if len(bytes.TrimSpace(data)) == 0 {
		r.problem("environment: %s: %s is empty", what, path)
		return nil
	}
	return data
}

func contains(list []string, value string) bool {
	for _, candidate := range list {
		if candidate == value {
			return true
		}
	}
	return false
}

// The Secrets the charts read their credentials from. The site-credentials
// item holds them, or the operator's secret store does when the credentials
// are external.
const (
	// credentialsItem is the manifest item added after the first item.
	credentialsItem = "site-credentials"
	// databaseSecret is read by the postgresql, temporal and controller items.
	databaseSecret = "nvke-database"
	// spireCASecret is the upstream CA Secret the spire item mounts.
	spireCASecret = "spire-upstream-ca"
)

// connectionStringKey is the key the controller reads its database URL from.
const connectionStringKey = "database-connection-string"

// siteSecret is one Secret the site-credentials item holds, or with external
// credentials one the cluster has to hold: with external credentials the
// values are empty, except the connection string's, which is its shape.
type siteSecret struct {
	namespace, name string
	data            map[string]string
}

// mergeSecrets joins the Secrets that share a namespace and a name, in the
// order each first appears, with the union of their keys. A Secret is one
// document: server-side apply gives a later document for the same Secret
// ownership of only its own keys and drops the rest.
func mergeSecrets(secrets []siteSecret) ([]siteSecret, error) {
	var merged []siteSecret
	index := map[string]int{}
	for _, sec := range secrets {
		id := sec.namespace + "/" + sec.name
		i, seen := index[id]
		if !seen {
			index[id] = len(merged)
			merged = append(merged, siteSecret{sec.namespace, sec.name, maps.Clone(sec.data)})
			continue
		}
		for k, v := range sec.data {
			if have, ok := merged[i].data[k]; ok && have != v {
				return nil, fmt.Errorf("the items that share the Secret %s give its key %s different values", id, k)
			}
			merged[i].data[k] = v
		}
	}
	return merged, nil
}

// expectedLine describes a Secret the cluster has to hold: namespace, name,
// keys and, for the connection string, the shape of its value.
func (s siteSecret) expectedLine() string {
	keys := make([]string, 0, len(s.data))
	for k := range s.data {
		if k != connectionStringKey {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	shape, hasURL := s.data[connectionStringKey]
	if hasURL {
		keys = append(keys, connectionStringKey)
	}
	line := "Secret " + s.namespace + "/" + s.name + " with keys " + strings.Join(keys, ", ")
	if hasURL {
		line += ", the URL " + shape + " with the password URL-escaped"
	}
	return line
}

// siteCredentials writes the site-credentials item, or with external
// credentials lists the Secrets it would hold. The items that read the same
// Secret name in one namespace share one Secret holding every key they read.
func (r *renderer) siteCredentials(password string, hasCA bool, caCert, caKey []byte) {
	for _, item := range r.items {
		if item.Name == credentialsItem {
			r.problem("%s: the deploy path already has an item of that name; the render adds it", credentialsItem)
			return
		}
	}
	namespaces := map[string]string{}
	for _, item := range []string{"postgresql", "temporal", "controller", "spire"} {
		ns, ok := r.itemNamespace(item)
		if !ok {
			return
		}
		namespaces[item] = ns
	}
	// With external credentials the password is empty and the controller's
	// value is the shape of the connection string.
	conn, ok := r.controllerConnectionString(password)
	if !ok {
		return
	}
	external := r.opts.Credentials == CredentialsExternal
	secrets := []siteSecret{
		{namespaces["postgresql"], databaseSecret, map[string]string{"postgres-password": password, "password": password}},
		{namespaces["temporal"], databaseSecret, map[string]string{"password": password}},
		{namespaces["controller"], databaseSecret, map[string]string{connectionStringKey: conn}},
	}
	if hasCA && (external || caCert != nil && caKey != nil) {
		// A self-signed root is its own bundle.
		secrets = append(secrets, siteSecret{namespaces["spire"], spireCASecret, map[string]string{"tls.crt": string(caCert), "tls.key": string(caKey), "bundle.crt": string(caCert)}})
	}
	secrets, err := mergeSecrets(secrets)
	if err != nil {
		r.problem("%s: %v", credentialsItem, err)
		return
	}
	if external {
		for _, sec := range secrets {
			r.out.Expected = append(r.out.Expected, sec.expectedLine())
		}
		return
	}
	var b bytes.Buffer
	b.WriteString("# Written by nvkectl render deploy-path: the Secrets the charts read.\n")
	for _, sec := range secrets {
		// data, not stringData: server-side apply cannot update stringData.
		data := map[string]string{}
		for k, v := range sec.data {
			data[k] = base64.StdEncoding.EncodeToString([]byte(v))
		}
		b.WriteString("---\n")
		doc := map[string]any{
			"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
			"metadata": map[string]any{"name": sec.name, "namespace": sec.namespace},
			"data":     data,
		}
		enc := yaml.NewEncoder(&b)
		enc.SetIndent(2)
		if err := enc.Encode(doc); err != nil {
			r.problem("%s: %v", credentialsItem, err)
			return
		}
	}
	if err := writePrivate(filepath.Join(r.opts.Out, credentialsItem, "secrets.yaml"), b.Bytes()); err != nil {
		r.problem("%s: %v", credentialsItem, err)
		return
	}
	r.credential(filepath.Join(credentialsItem, "secrets.yaml"))
	// After the first item: the namespaces exist, no chart has run.
	items := make([]Item, 0, len(r.items)+1)
	for i, item := range r.items {
		items = append(items, item)
		if i == 0 {
			items = append(items, Item{Name: credentialsItem, Type: TypeManifest})
		}
	}
	r.items = items
	r.itemsChanged = true
	r.note("%s: %d Secrets written", credentialsItem, len(secrets))
}

// itemNamespace is the release namespace in an item's release.yaml.
func (r *renderer) itemNamespace(item string) (string, bool) {
	rel, err := ReadRelease(filepath.Join(r.opts.DeployPath, item))
	if err != nil {
		r.problem("%s: %v", item, err)
		return "", false
	}
	if rel.Release == nil || rel.Release.Namespace == "" {
		r.problem("%s: release.yaml names no release namespace; the Secret it reads has nowhere to go", item)
		return "", false
	}
	return rel.Release.Namespace, true
}

// secretMeta is what the render reads of a rendered document.
type secretMeta struct {
	Kind     string `yaml:"kind"`
	Type     string `yaml:"type"`
	Metadata struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	StringData map[string]any `yaml:"stringData"`
	Data       map[string]any `yaml:"data"`
}

// yamlDocuments decodes a YAML stream into its documents; an empty document
// is dropped. Comments stay on the nodes, so a re-encoded document keeps them.
func yamlDocuments(stream []byte) ([]*yaml.Node, error) {
	dec := yaml.NewDecoder(bytes.NewReader(stream))
	var docs []*yaml.Node
	for {
		var node yaml.Node
		err := dec.Decode(&node)
		if errors.Is(err, io.EOF) {
			return docs, nil
		}
		if err != nil {
			return nil, err
		}
		if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
			docs = append(docs, &node)
		}
	}
}

// decodeSecretMeta reads one rendered document.
func decodeSecretMeta(doc *yaml.Node) (secretMeta, error) {
	var meta secretMeta
	if err := doc.Decode(&meta); err != nil {
		return meta, err
	}
	return meta, nil
}

// encodeDocuments writes docs as one YAML stream.
func encodeDocuments(docs []*yaml.Node) ([]byte, error) {
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	for _, doc := range docs {
		if err := enc.Encode(doc); err != nil {
			return nil, err
		}
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// expected records a Secret the operator's secret store must provide:
// namespace, name, keys and, unless Opaque, type.
func (r *renderer) expected(meta secretMeta) {
	keys := make([]string, 0, len(meta.StringData)+len(meta.Data))
	for k := range meta.StringData {
		keys = append(keys, k)
	}
	for k := range meta.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	line := fmt.Sprintf("Secret %s/%s with keys %s", meta.Metadata.Namespace, meta.Metadata.Name, strings.Join(keys, ", "))
	if meta.Type != "" && meta.Type != "Opaque" {
		line += ", type " + meta.Type
	}
	r.out.Expected = append(r.out.Expected, line)
}

// controllerConnectionString builds the controller's database URL from the
// committed appDb values and the password, escaped as userinfo. An empty
// password gives the shape, with <password> in its place.
func (r *renderer) controllerConnectionString(password string) (string, bool) {
	app := map[string]string{"url": "postgresql:5432", "dbName": "cloud-workflow", "postgresUser": "", "sslMode": "disable"}
	rel, err := ReadRelease(filepath.Join(r.opts.DeployPath, "controller"))
	if err != nil {
		r.problem("controller: %v", err)
		return "", false
	}
	for _, file := range rel.Values {
		data, err := os.ReadFile(filepath.Join(r.opts.DeployPath, "controller", file))
		if err != nil {
			r.problem("controller: %v", err)
			return "", false
		}
		// A node walk: helm tolerates a repeated key, a strict decode does not.
		var doc yaml.Node
		if err := yaml.Unmarshal(data, &doc); err != nil {
			r.problem("controller: %s: %v", file, err)
			return "", false
		}
		root := &doc
		if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
			root = root.Content[0]
		}
		if appDb := yamlChild(root, "appDb"); appDb != nil && appDb.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(appDb.Content); i += 2 {
				if v := appDb.Content[i+1]; v.Kind == yaml.ScalarNode && v.Value != "" {
					app[appDb.Content[i].Value] = v.Value
				}
			}
		}
	}
	if app["postgresUser"] == "" {
		r.problem("controller: appDb.postgresUser is not set in the deploy path's values")
		return "", false
	}
	userinfo := url.UserPassword(app["postgresUser"], password).String()
	if password == "" {
		userinfo = url.User(app["postgresUser"]).String() + ":<password>"
	}
	return fmt.Sprintf("postgresql://%s@%s/%s?sslmode=%s", userinfo, app["url"], app["dbName"], url.QueryEscape(app["sslMode"])), true
}
