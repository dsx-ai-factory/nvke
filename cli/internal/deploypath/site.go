// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package deploypath

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"fmt"
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

// DBPasswordFile is where render keeps a generated database password, so a
// later render can take it over through controllerStack.database.passwordFile.
const DBPasswordFile = "db-password"

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
// the BootstrapConfig field names docs/site-bootstrap/deploy-repository.md
// documents, filled from the site spec's controllerStack section.
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
	// Secrets land inside double-quoted YAML scalars and a JSON document in
	// the templates, which carry no escaping, so a value holding a quote, a
	// backslash or a control character is refused rather than written altered.
	read := func(what, path string) string {
		data, err := os.ReadFile(path)
		if err != nil {
			r.problem("site spec: %s: %v", what, err)
			return ""
		}
		value := strings.TrimSpace(string(data))
		if strings.ContainsAny(value, "\"\\") || strings.ContainsFunc(value, unicode.IsControl) {
			r.problem("site spec: %s: the value holds a character the templates cannot carry (a quote, a backslash or a control character)", what)
			return ""
		}
		return value
	}
	// controllerStack paths resolve from the site spec's directory; the
	// bootstrap's spire.ca*File resolve from the working directory, as
	// "nvkectl bootstrap site" reads them, so both commands embed one CA.
	stackPath := spec.ResolvePath
	var ctx TemplateContext
	ctx.ClusterName = spec.ClusterName
	ctx.Nico.Endpoint, ctx.Nico.OrgID, ctx.Nico.APIName = cs.Nico.Endpoint, cs.Nico.OrgID, cs.Nico.APIName
	ctx.Nico.Token = read("controllerStack.nico.tokenFile", stackPath(cs.Nico.TokenFile))
	ctx.Registry.Server, ctx.Registry.Username = cs.PullSecret.Server, cs.PullSecret.Username
	ctx.Registry.Token = read("controllerStack.pullSecret.tokenFile", stackPath(cs.PullSecret.TokenFile))
	ctx.Tenant.User = cs.Tenant.User
	ctx.Tenant.Password = read("controllerStack.tenant.passwordFile", stackPath(cs.Tenant.PasswordFile))
	password := r.dbPassword(spec)
	if len(r.problems) > 0 {
		return
	}
	trustDomain := spec.EffectiveTrustDomain(spec.ClusterName)
	checksum := controllerdeploy.SpireTPMDirectChecksum(cs.Arch)
	if checksum == "" {
		r.problem("site spec: controllerStack.arch %q has no TPM attestor checksum", cs.Arch)
		return
	}

	// controller: the database password, the OIDC discovery URL and the SPIRE
	// server, all VIP-derived; the images only when the site spec overrides them.
	controller := map[string]any{
		"appDb":       map[string]any{"postgresPassword": password},
		"nkeOIDCAuth": map[string]any{"discoveryURL": fmt.Sprintf("https://%s:%d/.well-known/openid-configuration", vip, oidcNodePort)},
		"spiffe":      map[string]any{"trustDomain": trustDomain, "spire": map[string]any{"serverAddress": vip}},
	}
	if cs.Images.Controller != "" {
		controller["image"] = imageValues(cs.Images.Controller)
	}
	if cs.Images.Profiles != "" {
		controller["profiles"] = map[string]any{"image": imageValues(cs.Images.Profiles)}
	}
	r.siteValues("controller", controller)

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
	if spec.Spire.CACertFile != "" && spec.Spire.CAKeyFile != "" {
		cert := readFile(r, "spire.caCertFile", spec.Spire.CACertFile)
		key := readFile(r, "spire.caKeyFile", spec.Spire.CAKeyFile)
		// A self-signed root is its own bundle.
		server["upstreamAuthority"] = map[string]any{"disk": map[string]any{
			"enabled": true,
			"secret":  map[string]any{"data": map[string]any{"certificate": cert, "key": key, "bundle": cert}},
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

	// postgresql and temporal share the one database password. The Bitnami
	// chart reads auth.postgresPassword for the superuser and auth.password
	// for the application user: one value for both.
	postgresql := map[string]any{"auth": map[string]any{"postgresPassword": password, "password": password}}
	if cs.Mirror != "" {
		// The chart refuses an image outside its own registry unless told so.
		postgresql["global"] = map[string]any{"imageRegistry": cs.Mirror, "security": map[string]any{"allowInsecureImages": true}}
	}
	r.siteValues("postgresql", postgresql)
	temporal := map[string]any{"server": map[string]any{"config": map[string]any{"persistence": map[string]any{
		"default":    map[string]any{"sql": map[string]any{"password": password}},
		"visibility": map[string]any{"sql": map[string]any{"password": password}},
	}}}}
	if cs.Mirror != "" {
		for key, repo := range temporalImages {
			temporal[key] = map[string]any{"image": map[string]any{"repository": cs.Mirror + "/" + repo}}
		}
	}
	r.siteValues("temporal", temporal)

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
		r.note("%s: not a helm item of this deploy path; no site values", item)
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

// dbPassword is the database password: the site spec's file, else a new one
// kept in the rendered tree for a later render to take over.
func (r *renderer) dbPassword(spec *sitespec.SiteSpec) string {
	if file := spec.ControllerStack.Database.PasswordFile; file != "" {
		data, err := os.ReadFile(spec.ResolvePath(file))
		if err != nil {
			r.problem("site spec: controllerStack.database.passwordFile: %v", err)
			return ""
		}
		return strings.TrimSpace(string(data))
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		r.problem("generating a database password: %v", err)
		return ""
	}
	password := base64.RawURLEncoding.EncodeToString(raw)
	if err := writePrivate(filepath.Join(r.opts.Out, DBPasswordFile), []byte(password+"\n")); err != nil {
		r.problem("%v", err)
		return ""
	}
	r.note("database password generated and kept in %s; name it as controllerStack.database.passwordFile to keep it on a later render", DBPasswordFile)
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
	rendered := 0
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
		// missingkey=error: a field the context does not have is a refused
		// render, not an empty Secret.
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
		if err := writePrivate(filepath.Join(dst, strings.TrimSuffix(name, ".tmpl")), out.Bytes()); err != nil {
			r.problem("%s: %v", item, err)
			return
		}
		if err := os.Remove(filepath.Join(dst, name)); err != nil {
			r.problem("%s: %v", item, err)
			return
		}
		rendered++
	}
	for i := range r.items {
		if r.items[i].Name == item {
			r.items[i].Type = TypeManifest
			r.itemsChanged = true
		}
	}
	r.note("%s: %d templates rendered; applies as a manifest item", item, rendered)
}

// imageValues splits an image reference into the chart's image values:
// repository and digest, or repository and tag. A tag beside a digest is
// dropped and tag is cleared so the overlay's tag does not survive as a
// second pin; the controller chart fills in its appVersion as a placeholder
// tag and the digest pins the pull.
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
		return map[string]any{"repository": ref[:colon], "tag": ref[colon+1:]}
	}
	return map[string]any{"repository": ref}
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

// readFile reads a file the site spec names, trimmed. A missing file is a
// problem naming the field.
func readFile(r *renderer, what, path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		r.problem("site spec: %s: %v", what, err)
		return ""
	}
	return strings.TrimSpace(string(data))
}

func contains(list []string, value string) bool {
	for _, candidate := range list {
		if candidate == value {
			return true
		}
	}
	return false
}
