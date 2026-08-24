// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type commonOptions struct {
	BaseURL string
	Org     string
	Token   string
}

func main() {
	if len(os.Args) < 3 {
		usage()
		os.Exit(2)
	}

	resource := os.Args[1]
	action := os.Args[2]
	args := os.Args[3:]

	switch resource {
	case "instance":
		runInstanceCommand(action, args)
	case "site":
		if action != "list" {
			usage()
			os.Exit(2)
		}
		opts, pageNumber, pageSize := parseCommonWithPage(args)
		exitOnErr(listResource(opts, "site", pageNumber, pageSize, map[string]string{}))
	case "instance-type":
		if action != "list" {
			usage()
			os.Exit(2)
		}
		opts, pageNumber, pageSize := parseCommonWithPage(args)
		siteID := requiredStringArg(args, "--site-id")
		exitOnErr(listResource(opts, "instance/type", pageNumber, pageSize, map[string]string{
			"siteId": siteID,
		}))
	default:
		usage()
		os.Exit(2)
	}
}

func runInstanceCommand(action string, args []string) {
	switch action {
	case "list":
		opts, pageNumber, pageSize := parseCommonWithPage(args)
		siteID := optionalStringArg(args, "--site-id")
		tenantID := optionalStringArg(args, "--tenant-id")
		q := map[string]string{}
		if siteID != "" {
			q["siteId"] = siteID
		}
		if tenantID != "" {
			q["tenantId"] = tenantID
		}
		exitOnErr(listResource(opts, "instance", pageNumber, pageSize, q))
	case "create":
		opts := parseCommon(args)
		name := requiredStringArg(args, "--name")
		tenantID := optionalStringArg(args, "--tenant-id")
		if tenantID == "" {
			tenantID = "d282929f-49d5-4dec-8d27-21ceb30ade19"
		}
		instanceTypeID := optionalStringArg(args, "--instance-type-id")
		if instanceTypeID == "" {
			instanceTypeID = "itype-mock-001"
		}
		vpcID := optionalStringArg(args, "--vpc-id")
		if vpcID == "" {
			vpcID = "vpc-mock-001"
		}
		interfaces := parseInterfaces(args)
		if len(interfaces) == 0 {
			interfaces = []map[string]string{{"subnetId": "subnet-mock-001"}}
		}
		nodeID := optionalStringArg(args, "--node-id")
		if nodeID == "" {
			nodeID = name
		}
		payload := map[string]interface{}{
			"name":           name,
			"tenantId":       tenantID,
			"instanceTypeId": instanceTypeID,
			"vpcId":          vpcID,
			"labels": map[string]string{
				"nke.nvidia.com/node-id": nodeID,
			},
			"sshKeyGroupIds": splitCSV(optionalStringArg(args, "--ssh-key-group-ids")),
			"interfaces":     interfaces,
		}
		if ipxe := optionalStringArg(args, "--ipxe-script"); ipxe != "" {
			payload["ipxeScript"] = ipxe
		}
		if userData := optionalStringArg(args, "--user-data"); userData != "" {
			payload["userData"] = userData
		}
		if hasBoolFlag(args, "--phone-home-enabled") {
			payload["phoneHomeEnabled"] = true
		}
		exitOnErr(doJSON(opts, http.MethodPost, buildPath(opts, "instance"), payload))
	case "update":
		opts := parseCommon(args)
		instanceID := requiredStringArg(args, "--instance-id")
		labels := parseKV(optionalStringArg(args, "--label"))
		payload := map[string]interface{}{
			"labels": labels,
		}
		if asBool(optionalStringArg(args, "--trigger-reboot")) {
			payload["triggerReboot"] = true
		}
		exitOnErr(doJSON(opts, http.MethodPatch, buildPath(opts, "instance/"+instanceID), payload))
	case "delete":
		opts := parseCommon(args)
		instanceID := requiredStringArg(args, "--instance-id")
		exitOnErr(doJSON(opts, http.MethodDelete, buildPath(opts, "instance/"+instanceID), map[string]interface{}{}))
	case "console-log":
		runConsoleLog(args)
	case "console-log-local":
		runConsoleLogLocal(args)
	default:
		usage()
		os.Exit(2)
	}
}

func parseCommon(args []string) commonOptions {
	baseURL := optionalStringArg(args, "--base-url")
	if baseURL == "" {
		baseURL = "http://localhost:18080"
	}
	org := optionalStringArg(args, "--org")
	if org == "" {
		org = "novxwn552ssc"
	}
	token := optionalStringArg(args, "--token")
	if token == "" {
		token = "nico-vmsctl-token"
	}
	return commonOptions{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Org:     org,
		Token:   token,
	}
}

func parseCommonWithPage(args []string) (commonOptions, int, int) {
	baseURL := optionalStringArg(args, "--base-url")
	if baseURL == "" {
		baseURL = "http://localhost:18080"
	}
	org := optionalStringArg(args, "--org")
	if org == "" {
		org = "novxwn552ssc"
	}
	token := optionalStringArg(args, "--token")
	if token == "" {
		token = "nico-vmsctl-token"
	}
	pageNumber := optionalIntArg(args, "--page-number", 1)
	pageSize := optionalIntArg(args, "--page-size", 100)
	return commonOptions{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Org:     org,
		Token:   token,
	}, pageNumber, pageSize
}

func listResource(opts commonOptions, resource string, pageNumber, pageSize int, query map[string]string) error {
	path := buildPath(opts, resource)
	u, err := url.Parse(path)
	if err != nil {
		return err
	}
	q := u.Query()
	q.Set("pageNumber", fmt.Sprintf("%d", pageNumber))
	q.Set("pageSize", fmt.Sprintf("%d", pageSize))
	q.Set("orderBy", "CREATED_ASC")
	for k, v := range query {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()
	return doJSON(opts, http.MethodGet, u.String(), nil)
}

func doJSON(opts commonOptions, method, endpoint string, payload interface{}) error {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}

	req, err := http.NewRequest(method, endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+opts.Token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	respBody, _ := io.ReadAll(res.Body)
	pretty := new(bytes.Buffer)
	if err := json.Indent(pretty, respBody, "", "  "); err == nil {
		fmt.Println(pretty.String())
	} else {
		fmt.Println(string(respBody))
	}

	if res.StatusCode >= 400 {
		return fmt.Errorf("request failed: %s", res.Status)
	}
	return nil
}

func parseInterfaces(args []string) []map[string]string {
	raw := optionalStringArg(args, "--interfaces")
	if raw == "" {
		subnetID := optionalStringArg(args, "--subnet-id")
		vpcPrefixID := optionalStringArg(args, "--vpc-prefix-id")
		if subnetID != "" {
			return []map[string]string{{"subnetId": subnetID}}
		}
		if vpcPrefixID != "" {
			return []map[string]string{{"vpcPrefixId": vpcPrefixID}}
		}
		return nil
	}

	items := []map[string]string{}
	for _, chunk := range strings.Split(raw, ",") {
		chunk = strings.TrimSpace(chunk)
		if chunk == "" {
			continue
		}
		if strings.HasPrefix(chunk, "subnet:") {
			items = append(items, map[string]string{"subnetId": strings.TrimPrefix(chunk, "subnet:")})
			continue
		}
		if strings.HasPrefix(chunk, "vpcPrefix:") {
			items = append(items, map[string]string{"vpcPrefixId": strings.TrimPrefix(chunk, "vpcPrefix:")})
		}
	}
	return items
}

func parseKV(raw string) map[string]string {
	out := map[string]string{}
	if raw == "" {
		return out
	}
	for _, pair := range strings.Split(raw, ",") {
		parts := strings.SplitN(strings.TrimSpace(pair), "=", 2)
		if len(parts) != 2 {
			continue
		}
		out[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
	}
	return out
}

func splitCSV(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		v := strings.TrimSpace(p)
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

func asBool(v string) bool {
	return strings.EqualFold(v, "true") || v == "1" || strings.EqualFold(v, "yes")
}

func buildPath(opts commonOptions, suffix string) string {
	return fmt.Sprintf("%s/v2/org/%s/forge/%s", opts.BaseURL, opts.Org, strings.TrimPrefix(suffix, "/"))
}

func requiredStringArg(args []string, name string) string {
	v := optionalStringArg(args, name)
	if v == "" {
		fmt.Fprintf(os.Stderr, "missing required flag %s\n", name)
		os.Exit(2)
	}
	return v
}

func hasBoolFlag(args []string, name string) bool {
	for _, a := range args {
		if a == name {
			return true
		}
	}
	return false
}

func optionalStringArg(args []string, name string) string {
	prefix := name + "="
	for i := 0; i < len(args); i++ {
		if args[i] == name && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(args[i], prefix) {
			return strings.TrimPrefix(args[i], prefix)
		}
	}
	return ""
}

func optionalIntArg(args []string, name string, defaultValue int) int {
	raw := optionalStringArg(args, name)
	if raw == "" {
		return defaultValue
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid integer value for %s: %q\n", name, raw)
		os.Exit(2)
	}
	return value
}

// hasFlag returns true if the given boolean flag (e.g. "-f") is present in args.
func hasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == name {
			return true
		}
	}
	return false
}

func exitOnErr(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(`nico-vmsctl commands:
  nico-vmsctl instance list --base-url URL --org ORG --token TOKEN [--site-id ID] [--tenant-id ID]
  nico-vmsctl instance create --base-url URL --org ORG --token TOKEN --name NAME --tenant-id ID --instance-type-id ID --vpc-id ID (--subnet-id ID | --vpc-prefix-id ID | --interfaces "subnet:ID,vpcPrefix:ID")
  nico-vmsctl instance update --base-url URL --org ORG --token TOKEN --instance-id ID [--label k=v,k2=v2] [--trigger-reboot true]
  nico-vmsctl instance delete --base-url URL --org ORG --token TOKEN --instance-id ID
  nico-vmsctl instance console-log (--name NAME | --id ID) [--dump] [--base-url URL] [--org ORG] [--token TOKEN] [--site-id ID]
  nico-vmsctl instance console-log-local [--name NAME | --id ID] [--dir DIR] [--dump] [--lines N]
  nico-vmsctl instance-type list --base-url URL --org ORG --token TOKEN --site-id ID
  nico-vmsctl site list --base-url URL --org ORG --token TOKEN`)
}
