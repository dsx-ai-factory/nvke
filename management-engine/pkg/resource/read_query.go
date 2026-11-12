// SPDX-License-Identifier: Apache-2.0

package resource

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/selection"

	"github.com/dsx-ai-factory/nvke/management-engine/pkg/projection"
)

func invalidSelector() error {
	return Invalid(FieldError{
		Field: fieldSelectorParameter, Code: invalidCode, Message: "Use equality selectors on supported fields.",
	})
}

// validateFields prevents selectors from reaching internal or dynamically typed fields.
func validateFields[E any](names []string) error {
	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] || !stringField(reflect.TypeFor[E](), strings.Split(name, ".")) {
			return fmt.Errorf("selectable field %q must identify a unique public string field", name)
		}
		seen[name] = true
	}
	return nil
}

// stringField uses declared types because JSON decoders can accept strings for non-string fields.
func stringField(t reflect.Type, path []string) bool {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if len(path) == 0 {
		return t.Kind() == reflect.String
	}
	if t.Kind() != reflect.Struct {
		return false
	}
	for field := range t.Fields() {
		if !field.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if field.Anonymous && name == "" {
			if stringField(field.Type, path) {
				return true
			}
		} else {
			if name == "" {
				name = field.Name
			}
			if name == path[0] && stringField(field.Type, path[1:]) {
				return true
			}
		}
	}
	return false
}

// listSelector validates bounded public controls for direct callers as well as HTTP callers.
func listSelector(opts *ListOptions, supported []string) (fields.Selector, error) {
	if opts.PageSize == 0 {
		opts.PageSize = 100
	}
	if opts.PageSize < 1 || opts.PageSize > 500 {
		return nil, Invalid(FieldError{
			Field: pageSizeParameter, Code: invalidCode, Message: "Page size must be between 1 and 500.",
		})
	}
	if len(opts.PageKey) > maxPageKeyBytes {
		return nil, Invalid(FieldError{Field: pageKeyParameter, Code: invalidCode, Message: "The page key is too long."})
	}
	if len(opts.FieldSelector) > 4096 {
		return nil, invalidSelector()
	}
	selector, err := fields.ParseSelector(opts.FieldSelector)
	if err != nil || opts.FieldSelector != "" && selector.Empty() {
		return nil, invalidSelector()
	}
	seen := map[string]bool{}
	for _, requirement := range selector.Requirements() {
		if requirement.Operator != selection.Equals ||
			!slices.Contains(supported, requirement.Field) || seen[requirement.Field] {
			return nil, invalidSelector()
		}
		seen[requirement.Field] = true
	}
	return selector, nil
}

type viewCursor struct {
	After    string `json:"after"`
	Binding  string `json:"binding"`
	Revision string `json:"revision"`
}

// viewPage binds continuations to the query, trusted scope and complete projected result, never to a backend page.
func viewPage[E namedView](views []E, opts ListOptions, selector fields.Selector, identity string) (List[E], error) {
	page := List[E]{Items: []E{}}
	filtered := make([]E, 0, len(views))
	for _, value := range views {
		matches, err := matchesView(value, selector)
		if err != nil {
			return page, err
		}
		if matches {
			filtered = append(filtered, value)
		}
	}
	slices.SortFunc(filtered, func(a, b E) int { return strings.Compare(a.GetName(), b.GetName()) })
	encodedViews, err := json.Marshal(filtered)
	if err != nil {
		return page, projectionFailure(err)
	}
	current := viewCursor{
		Binding: hashCatalogState([]byte(identity + "/" + selector.String())), Revision: hashCatalogState(encodedViews),
	}
	start := 0
	if opts.PageKey != "" {
		var previous viewCursor
		encodedCursor, err := base64.RawURLEncoding.DecodeString(opts.PageKey)
		if err != nil || json.Unmarshal(encodedCursor, &previous) != nil ||
			previous.Binding != current.Binding || previous.After == "" {
			return page, Invalid(FieldError{Field: pageKeyParameter, Code: invalidCode, Message: invalidPageKeyMessage})
		}
		if previous.Revision != current.Revision {
			return page, problem(http.StatusGone, pageExpiredCode, "The page has expired.")
		}
		var found bool
		start, found = slices.BinarySearchFunc(filtered, previous.After, func(value E, name string) int {
			return strings.Compare(value.GetName(), name)
		})
		if !found {
			return page, Invalid(FieldError{Field: pageKeyParameter, Code: invalidCode, Message: invalidPageKeyMessage})
		}
		start++
	}
	end := min(start+opts.PageSize, len(filtered))
	page.Items = filtered[start:end]
	page.HasMore = end < len(filtered)
	if page.HasMore {
		current.After = filtered[end-1].GetName()
		encodedCursor, err := json.Marshal(current)
		if err != nil {
			return page, err
		}
		page.PageKey = base64.RawURLEncoding.EncodeToString(encodedCursor)
	}
	return page, nil
}

// matchesView evaluates selectors on the public projection, never the backing resource.
func matchesView[E any](value E, selector fields.Selector) (bool, error) {
	if selector.Empty() {
		return true, nil
	}
	object, err := projection.JSON[map[string]any](value)
	if err != nil {
		return false, projectionFailure(err)
	}
	values := fields.Set{}
	for _, requirement := range selector.Requirements() {
		selected, _, err := unstructured.NestedString(object, strings.Split(requirement.Field, ".")...)
		if err != nil {
			return false, projectionFailure(fmt.Errorf("read selected field %q: %w", requirement.Field, err))
		}
		values[requirement.Field] = selected
	}
	return selector.Matches(values), nil
}

func hashCatalogState(state []byte) string {
	sum := sha256.Sum256(state)
	return hex.EncodeToString(sum[:])
}
