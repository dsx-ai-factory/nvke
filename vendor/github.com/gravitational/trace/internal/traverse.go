// Copyright 2026 Gravitational, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package internal

// TraverseErr traverses the err error chain until fn returns true.
// Traversal stops on nil errors, fn(nil) is never called.
// Returns true if fn matched, false otherwise.
func TraverseErr(err error, fn func(error) (ok bool)) (ok bool) {
	if err == nil {
		return false
	}

	if fn(err) {
		return true
	}

	switch err := err.(type) {
	case interface{ Unwrap() error }:
		return TraverseErr(err.Unwrap(), fn)

	case interface{ Unwrap() []error }:
		for _, err2 := range err.Unwrap() {
			if TraverseErr(err2, fn) {
				return true
			}
		}
	}

	return false
}
