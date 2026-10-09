// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nke

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/dsx-ai-factory/nke/controller/api"
)

const (
	// nodeLabelsField is the one response field whose JSON shape differs
	// between the controller releases this suite qualifies.
	nodeLabelsField = "nodeLabels"
)

// labelShapeCompat reshapes node labels coming back from a controller older
// than #1306, which changed them from a list of "key=value" strings to an
// object.
//
// The suite's API client is generated from the checkout, while the controller
// it talks to is the release under qualification, so the two disagree across
// that change and there is no version of the client that is right for both.
// NodePool declares nodeLabels required, so a pre-#1306 controller sends
// "nodeLabels": [] on every node-pool list and the decode fails before any
// field is read -- including on the readiness path, which only looks at pool
// names.
//
// Responses only. Nothing in the suite sets node labels on a request, and the
// field is omitempty everywhere it is sent, so requests carry the same bytes
// against either controller.
//
// Doing this at the transport rather than in the model keeps the generated
// code generated and the controller untouched, and puts the whole
// accommodation in one file to delete once the oldest release the lane
// qualifies carries #1306.
type labelShapeCompat struct{ next api.HttpRequestDoer }

func (c labelShapeCompat) Do(req *http.Request) (*http.Response, error) {
	resp, err := c.next.Do(req)
	if err != nil || resp.Body == nil {
		return resp, err
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		return resp, nil
	}

	body, readErr := io.ReadAll(resp.Body)
	closeErr := resp.Body.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))

	// A body this cannot parse, or finds nothing to reshape in, is handed on
	// as it arrived: the caller's own decode gives the better error, and a
	// current controller must not be re-encoded on its way through.
	var document any
	if json.Unmarshal(body, &document) != nil || !labelsToObjects(document) {
		return resp, nil
	}
	reshaped, err := json.Marshal(document)
	if err != nil {
		return resp, nil
	}
	resp.Body = io.NopCloser(bytes.NewReader(reshaped))
	resp.ContentLength = int64(len(reshaped))
	resp.Header.Set("Content-Length", strconv.Itoa(len(reshaped)))
	return resp, nil
}

// labelsToObjects rewrites every list-shaped nodeLabels in document, at any
// depth, and reports whether it rewrote any. Depth rather than a known path
// because nodeLabels appears on node pools, on control-plane config and on
// the cluster responses that embed both.
func labelsToObjects(document any) bool {
	rewrote := false
	switch node := document.(type) {
	case map[string]any:
		for key, value := range node {
			if key == nodeLabelsField {
				if object, ok := labelListToObject(value); ok {
					node[key] = object
					rewrote = true
					continue
				}
			}
			rewrote = labelsToObjects(value) || rewrote
		}
	case []any:
		for _, item := range node {
			rewrote = labelsToObjects(item) || rewrote
		}
	}
	return rewrote
}

// labelListToObject turns ["k=v"] into {"k": "v"}. It reports false for
// anything that is not a list of strings, so labels already an object -- the
// current controller -- pass through untouched.
func labelListToObject(value any) (map[string]string, bool) {
	list, ok := value.([]any)
	if !ok {
		return nil, false
	}
	object := make(map[string]string, len(list))
	for _, item := range list {
		label, ok := item.(string)
		if !ok {
			return nil, false
		}
		// A bare key is a label with an empty value, which is how both the
		// older controller and kubectl spell one.
		key, labelValue, _ := strings.Cut(label, "=")
		object[key] = labelValue
	}
	return object, true
}

// SchemaMismatch reports whether err is the client and the controller
// disagreeing about the shape of a response.
//
// Worth telling apart because it is permanent: a poll that waits for it to
// clear waits for its whole budget and then reports a timeout rather than the
// disagreement. The readiness spec of [run 37854934923] spent 45 minutes on a
// GB200 doing exactly that.
//
// [run 37854934923]: https://github.com/dsx-ai-factory/nke/actions/runs/37854934923
func SchemaMismatch(err error) bool {
	var typeErr *json.UnmarshalTypeError
	return errors.As(err, &typeErr)
}
