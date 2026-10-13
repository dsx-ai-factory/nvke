// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package deploypath

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"unicode"

	"gopkg.in/yaml.v3"

	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/controllerdeploy"
	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/sitespec"
)

// SiteValuesFile is the values file render appends to an item for the site.
const SiteValuesFile = "site-values.yaml"

// oidcNodePort is the NodePort the SPIRE OIDC discovery provider is published
// on, by the spire-oidc-provider-nodeport item.
const oidcNodePort = 30443

// controllerAdminPath is the SPIFFE path the controller presents:
// /nke-service/<ServiceAccount>, and the deploy path names the ServiceAccount
// nke-controller. It must match spire-server.adminIDs in spire/values.yaml.
const controllerAdminPath = "/nke-service/nke-controller"

// temporalImages are the repositories the Temporal chart defaults to; the
// chart has no registry key, so a mirror is written into the repository.
var temporalImages = map[string]string{"server": "temporalio/server", "admintools": "temporalio/admin-tools", "web": "temporalio/ui"}

// TemplateContext is what a template item's .yaml.tmpl files render against:
// the subset of the BootstrapConfig fields in
// docs/site-bootstrap/deploy-repository.md that the site spec's
// controllerStack section supplies. A template naming another field is
// refused.
type TemplateContext struct {
	ClusterName string
	Nico        struct{ Endpoint, OrgID, Token, APIName string }
	Registry    struct{ Server, Username, Token string }
	Tenant      struct{ User, Password string }
}

// site writes the site values and renders the template items.
func (r *renderer) site() {
	spec := r.opts.Site
	if err := spec.ValidateControllerStack(); err != nil {
		r.problem("site spec: %v", err)
		return
	}
	if err := spec.ValidateControllerStackReferences(); err != nil {
		r.problem("site spec: %v", err)
		return
	}
	cs := spec.ControllerStack
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
			r.problem("site spec: %s is empty", what)
			return ""
		}
		if strings.ContainsAny(value, "\"\\") || strings.ContainsFunc(value, unicode.IsControl) {
			r.problem("site spec: %s: the value holds a character the templates cannot carry (a quote, a backslash or a control character)", what)
			return ""
		}
		return value
	}
	read := func(what, path string) string {
		data, err := os.ReadFile(path)
		if err != nil {
			r.problem("site spec: %s: %v", what, err)
			return ""
		}
		return check(what, strings.TrimSpace(string(data)))
	}
	// controllerStack paths resolve from the site spec's directory; the
	// bootstrap's spire.ca*File resolve from the working directory, as
	// "nvkectl bootstrap site" reads them, so both commands embed one CA.
	stackPath := spec.ResolvePath
	var ctx TemplateContext
	ctx.ClusterName = spec.ClusterName
	ctx.Nico.Endpoint = check("controllerStack.nico.endpoint", cs.Nico.Endpoint)
	ctx.Nico.OrgID = check("controllerStack.nico.orgID", cs.Nico.OrgID)
	ctx.Nico.APIName = check("controllerStack.nico.apiName", cs.Nico.APIName)
	ctx.Nico.Token = read("controllerStack.nico.tokenFile", stackPath(cs.Nico.TokenFile))
	ctx.Registry.Server = check("controllerStack.pullSecret.server", cs.PullSecret.Server)
	ctx.Registry.Username = check("controllerStack.pullSecret.username", cs.PullSecret.Username)
	ctx.Registry.Token = read("controllerStack.pullSecret.tokenFile", stackPath(cs.PullSecret.TokenFile))
	ctx.Tenant.User = check("controllerStack.tenant.user", cs.Tenant.User)
	ctx.Tenant.Password = read("controllerStack.tenant.passwordFile", stackPath(cs.Tenant.PasswordFile))
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
	checksum := controllerdeploy.SpireTPMDirectChecksum(cs.Arch)
	if checksum == "" {
		r.problem("site spec: controllerStack.arch %q has no TPM attestor checksum", cs.Arch)
		return
	}
	// May write the operator's password file. A later refusal leaves it in
	// place: it is the operator's, and the next render reads it back.
	password := r.dbPassword(spec)
	if len(r.problems) > 0 {
		return
	}

	// controller: the database password, the OIDC discovery URL and the SPIRE
	// server, all VIP-derived; the images only when the site spec overrides them.
	controller := map[string]any{
		"appDb": map[string]any{"postgresPassword": password},
		// The chart then writes the connection string, URL-escaped, into a
		// Secret instead of a plain env value.
		"secrets":     map[string]any{"createDbSecret": true},
		"nkeOIDCAuth": map[string]any{"discoveryURL": fmt.Sprintf("https://%s:%d/.well-known/openid-configuration", vip, oidcNodePort)},
		"spiffe":      map[string]any{"trustDomain": trustDomain, "spire": map[string]any{"serverAddress": vip}},
	}
	if cs.Images.Controller != "" {
		controller["image"] = r.imageValues("controllerStack.images.controller", cs.Images.Controller)
		if r.opts.Release != nil {
			r.note("controller: controllerStack.images.controller overrides the release's image")
		}
	}
	if cs.Images.Profiles != "" {
		controller["profiles"] = map[string]any{"image": r.imageValues("controllerStack.images.profiles", cs.Images.Profiles)}
		if r.opts.Release != nil {
			r.note("controller: controllerStack.images.profiles overrides the release's profiles image")
		}
	} else if r.opts.Release == nil && r.profilesImageRequired() {
		// With a release, the controller item's profilesArtifact names it.
		r.problem("site spec: controllerStack.images.profiles is required: the deploy path enables profiles.image and nothing else names the image")
	}
	r.siteValues("controller", controller)
	r.credential(filepath.Join("controller", SiteValuesFile))

	// spire: site identity, the controller's admin ID, the TPM attestor
	// checksum for the architecture, the bootstrap CA when the spec names
	// one, and the OIDC provider published on the VIP.
	global := map[string]any{"trustDomain": trustDomain, "clusterName": spec.ClusterName}
	if cs.Mirror != "" {
		global["image"] = map[string]any{"registry": cs.Mirror}
	}
	server := map[string]any{
		"adminIDs":     []string{"spiffe://" + trustDomain + controllerAdminPath},
		"nodeAttestor": map[string]any{"tpmDirect": map[string]any{"checksum": checksum}},
	}
	// The CA files stay with the operator: helm reads them at install time
	// through chart.setFile. A self-signed root is its own bundle.
	var caFiles []string
	if spec.Spire.CACertFile != "" && spec.Spire.CAKeyFile != "" {
		cert := secretPath(r, "spire.caCertFile", spec.Spire.CACertFile)
		key := secretPath(r, "spire.caKeyFile", spec.Spire.CAKeyFile)
		server["upstreamAuthority"] = map[string]any{"disk": map[string]any{"enabled": true}}
		const prefix = "spire-server.upstreamAuthority.disk.secret.data."
		caFiles = []string{prefix + "certificate=" + cert, prefix + "key=" + key, prefix + "bundle=" + cert}
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
	if len(caFiles) > 0 && len(r.problems) == 0 {
		r.appendSetFile("spire", caFiles)
		r.note("spire: the CA is read from %s and %s at install time", spec.Spire.CACertFile, spec.Spire.CAKeyFile)
	}

	// postgresql and temporal share the one database password. The Bitnami
	// chart reads auth.postgresPassword for the superuser and auth.password
	// for the application user: one value for both.
	postgresql := map[string]any{"auth": map[string]any{"postgresPassword": password, "password": password}}
	if cs.Mirror != "" {
		// The chart refuses an image outside its own registry unless told so.
		postgresql["global"] = map[string]any{"imageRegistry": cs.Mirror, "security": map[string]any{"allowInsecureImages": true}}
	}
	r.siteValues("postgresql", postgresql)
	r.credential(filepath.Join("postgresql", SiteValuesFile))
	temporal := map[string]any{"server": map[string]any{"config": map[string]any{"persistence": map[string]any{
		"default":    map[string]any{"sql": map[string]any{"password": password}},
		"visibility": map[string]any{"sql": map[string]any{"password": password}},
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
	r.credential(filepath.Join("temporal", SiteValuesFile))

	// nktp: the operator's values file, passed through.
	if cs.NktpValuesFile != "" {
		if !r.hasItem("nktp", TypeHelm) {
			r.problem("site spec: controllerStack.nktpValuesFile is set, but the deploy path has no nktp helm item")
		} else if data, err := os.ReadFile(stackPath(cs.NktpValuesFile)); err != nil {
			r.problem("site spec: controllerStack.nktpValuesFile: %v", err)
		} else {
			r.appendValues("nktp", SiteValuesFile, data)
			r.note("nktp: values from %s", cs.NktpValuesFile)
		}
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

// dbPassword is the database password from controllerStack.database.passwordFile.
// A missing file is created with a generated password, so the operator's
// file is the only copy.
func (r *renderer) dbPassword(spec *sitespec.SiteSpec) string {
	file := spec.ControllerStack.Database.PasswordFile
	if file == "" {
		r.problem("site spec: controllerStack.database.passwordFile is required; the render keeps no password")
		return ""
	}
	path := spec.ResolvePath(file)
	data, err := os.ReadFile(path)
	if err == nil {
		if value := strings.TrimSpace(string(data)); value != "" {
			return value
		}
		r.problem("site spec: controllerStack.database.passwordFile: %s is empty", path)
		return ""
	}
	if !errors.Is(err, fs.ErrNotExist) {
		r.problem("site spec: controllerStack.database.passwordFile: %v", err)
		return ""
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		r.problem("generating a database password: %v", err)
		return ""
	}
	password := base64.RawURLEncoding.EncodeToString(raw)
	if err := writePrivate(path, []byte(password+"\n")); err != nil {
		r.problem("site spec: controllerStack.database.passwordFile: %v", err)
		return ""
	}
	// Said at once, not noted: the file exists whether or not the render goes on.
	fmt.Fprintf(r.opts.Log, "database password generated and written to %s\n", path)
	return password
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
		if err := writePrivate(filepath.Join(dst, rendered), out.Bytes()); err != nil {
			r.problem("%s: %v", item, err)
			return
		}
		if bytes.Contains(out.Bytes(), []byte("kind: Secret")) {
			r.credential(filepath.Join(item, rendered))
		}
		if err := os.Remove(filepath.Join(dst, name)); err != nil {
			r.problem("%s: %v", item, err)
			return
		}
		count++
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
		r.problem("site spec: %s: %q names neither a tag nor a digest", what, ref)
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
// enable profiles.image without naming a tag or digest, so the site spec
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

// secretPath returns the absolute path of a file the site spec names for
// the install to read. A missing file, or a path the deploy path reader
// cannot carry, is a problem naming the field.
func secretPath(r *renderer, what, path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		r.problem("site spec: %s: %v", what, err)
		return ""
	}
	if _, err := os.Stat(abs); err != nil {
		r.problem("site spec: %s: %v", what, err)
		return ""
	}
	if strings.ContainsFunc(abs, unicode.IsSpace) {
		r.problem("site spec: %s: the path %q holds whitespace, which the deploy path reader cannot carry; move the file to a path without spaces", what, abs)
		return ""
	}
	return abs
}

func contains(list []string, value string) bool {
	for _, candidate := range list {
		if candidate == value {
			return true
		}
	}
	return false
}
