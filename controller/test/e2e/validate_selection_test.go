// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"errors"
	"flag"
	"fmt"
	"regexp"
	"strings"
)

// The validation image chooses what a run covers with two switches rather
// than a label filter a caller has to write: the site, a tenant, or both.
// Each selects the specs carrying its label. Specs that belong to both, such
// as the controller health specs a tenant's checks rely on, carry both labels.
const (
	siteValidateLabel   = "site-validate"
	tenantValidateLabel = "tenant-validate"

	// siteValidateEnv selects the site-validate specs. The image sets it.
	siteValidateEnv = "NVKE_SITE_VALIDATE"
	// tenantValidateEnv selects the tenant-validate specs. The image sets it
	// false, so a run validates the site alone unless it asks for a tenant.
	tenantValidateEnv = "NVKE_TENANT_VALIDATE"

	labelFilterFlag = "ginkgo.label-filter"

	// validateExcludeLabelsEnv lists, comma-separated, labels whose specs a
	// run leaves out, on top of whatever else selects specs. For a site where
	// a check does not apply, such as issuesCertificateLabel where
	// cert-manager approves only what a CertificateRequestPolicy allows.
	validateExcludeLabelsEnv = "NVKE_VALIDATE_EXCLUDE_LABELS"

	// issuesCertificateLabel is the one creates spec that needs cert-manager
	// to approve a new self-signed Issuer's request, so it can be left out
	// without the other creates specs.
	issuesCertificateLabel = "issues-certificate"
)

// excludableLabel is what a Ginkgo label may be without changing what a
// filter means: no operator, separator, grouping or space.
var excludableLabel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)

// validationExclusions reads validateExcludeLabelsEnv. It refuses the two
// labels that select a validation run at all: leaving either out is turning
// the run off, which is the switches' job.
func validationExclusions(getenv func(string) string) ([]string, error) {
	labels := splitList(getenv(validateExcludeLabelsEnv), ",")
	for _, label := range labels {
		if !excludableLabel.MatchString(label) {
			return nil, fmt.Errorf("%s: %q is not a label", validateExcludeLabelsEnv, label)
		}
		if label == siteValidateLabel || label == tenantValidateLabel {
			return nil, fmt.Errorf("%s: %s turns the run off; set %s or %s instead", validateExcludeLabelsEnv, label, siteValidateEnv, tenantValidateEnv)
		}
	}
	return labels, nil
}

// withExclusions ANDs the exclusions onto filter, which may be empty.
func withExclusions(filter string, exclude []string) string {
	if len(exclude) == 0 {
		return filter
	}
	var terms []string
	if filter != "" {
		terms = append(terms, "("+filter+")")
	}
	for _, label := range exclude {
		terms = append(terms, "!"+label)
	}
	return strings.Join(terms, " && ")
}

// validationLabelFilter is the label filter the switches choose. It returns
// "" when neither is present, as in any run outside the image, so the suite
// runs as it always has. A switch that is present but blank reads as false:
// a run that blanks both validates nothing and fails, rather than running
// the whole suite, specs that change the cluster included.
func validationLabelFilter(lookupEnv func(string) (string, bool)) (string, error) {
	rawSite, siteSet := lookupEnv(siteValidateEnv)
	rawTenant, tenantSet := lookupEnv(tenantValidateEnv)
	if !siteSet && !tenantSet {
		return "", nil
	}
	site, err := parseRequireEnv(siteValidateEnv, rawSite)
	if err != nil {
		return "", err
	}
	tenant, err := parseRequireEnv(tenantValidateEnv, rawTenant)
	if err != nil {
		return "", err
	}

	var labels []string
	if site {
		labels = append(labels, siteValidateLabel)
	}
	if tenant {
		labels = append(labels, tenantValidateLabel)
	}
	if len(labels) == 0 {
		return "", errors.New(siteValidateEnv + " and " + tenantValidateEnv + " are both false, so the run would validate nothing")
	}
	return strings.Join(labels, " || "), nil
}

// labelFilterGiven reports whether the caller passed -ginkgo.label-filter. An
// explicit filter wins over the switches, so the suite stays selectable the
// way Ginkgo documents.
func labelFilterGiven() bool {
	given := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == labelFilterFlag {
			given = true
		}
	})
	return given
}
