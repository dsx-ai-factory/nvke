// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// TestConstantTimeCompareAcceptsTwoEmptyStrings pins the standard-library
// behavior this package has to defend against.
//
// It is not testing our code. It is here so that if the premise ever changes,
// the reason for the guards in UnaryInterceptor is still discoverable rather
// than looking like defensive noise someone can delete.
func TestConstantTimeCompareAcceptsTwoEmptyStrings(t *testing.T) {
	if got := subtle.ConstantTimeCompare([]byte(""), []byte("")); got != 1 {
		t.Fatalf("premise changed: ConstantTimeCompare(\"\", \"\") = %d, want 1", got)
	}
}

func writeTokenFile(t *testing.T, contents string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing token file: %v", err)
	}
	return path
}

// callWith runs the interceptor with the given authorization metadata and
// reports whether the request reached the handler, plus the gRPC status code.
//
// Reaching the handler is the assertion that matters. A test that only checked
// the code would pass on a handler that ran and then returned an error.
func callWith(t *testing.T, a *TokenAuthenticator, md metadata.MD) (bool, codes.Code) {
	t.Helper()

	ctx := context.Background()
	if md != nil {
		ctx = metadata.NewIncomingContext(ctx, md)
	}

	reached := false
	handler := func(context.Context, any) (any, error) {
		reached = true
		return "ok", nil
	}

	_, err := a.UnaryInterceptor()(ctx, nil, &grpc.UnaryServerInfo{}, handler)
	return reached, status.Code(err)
}

func TestNewTokenAuthenticator(t *testing.T) {
	tests := []struct {
		name     string
		contents string
		wantErr  error
	}{
		{name: "a real token", contents: "s3cret", wantErr: nil},
		{name: "trailing newline is trimmed", contents: "s3cret\n", wantErr: nil},
		// The defect this package existed with: an empty file loaded fine and
		// the server then authenticated anyone presenting nothing.
		{name: "empty file", contents: "", wantErr: ErrEmptyToken},
		{name: "whitespace only", contents: "   \n\t ", wantErr: ErrEmptyToken},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := NewTokenAuthenticator(writeTokenFile(t, tt.contents))
			if a != nil {
				t.Cleanup(a.Close)
			}

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				if a != nil {
					t.Error("an authenticator was returned alongside an error; it must be nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if a.token != "s3cret" {
				t.Errorf("token = %q, want %q", a.token, "s3cret")
			}
		})
	}
}

func TestNewTokenAuthenticatorMissingFile(t *testing.T) {
	a, err := NewTokenAuthenticator(filepath.Join(t.TempDir(), "does-not-exist"))
	if a != nil {
		t.Cleanup(a.Close)
		t.Error("an authenticator was returned for a missing file")
	}
	if err == nil {
		t.Fatal("expected an error for a missing token file")
	}
}

func TestUnaryInterceptor(t *testing.T) {
	a, err := NewTokenAuthenticator(writeTokenFile(t, "s3cret"))
	if err != nil {
		t.Fatalf("constructing authenticator: %v", err)
	}
	t.Cleanup(a.Close)

	tests := []struct {
		name        string
		md          metadata.MD
		wantReached bool
		wantCode    codes.Code
	}{
		{
			name:        "correct token",
			md:          metadata.Pairs("authorization", "Bearer s3cret"),
			wantReached: true,
			wantCode:    codes.OK,
		},
		{
			name:     "wrong token",
			md:       metadata.Pairs("authorization", "Bearer wrong"),
			wantCode: codes.Unauthenticated,
		},
		{
			name:     "empty bearer token",
			md:       metadata.Pairs("authorization", "Bearer "),
			wantCode: codes.Unauthenticated,
		},
		{
			name:     "empty authorization header",
			md:       metadata.Pairs("authorization", ""),
			wantCode: codes.Unauthenticated,
		},
		{
			name:     "no authorization header",
			md:       metadata.Pairs("other", "value"),
			wantCode: codes.Unauthenticated,
		},
		{
			name:     "no metadata at all",
			md:       nil,
			wantCode: codes.Unauthenticated,
		},
		{
			name:     "a prefix of the token",
			md:       metadata.Pairs("authorization", "Bearer s3cre"),
			wantCode: codes.Unauthenticated,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reached, code := callWith(t, a, tt.md)

			if reached != tt.wantReached {
				t.Errorf("handler reached = %v, want %v", reached, tt.wantReached)
			}
			if code != tt.wantCode {
				t.Errorf("code = %v, want %v", code, tt.wantCode)
			}
		})
	}
}

// TestZeroValueAuthenticatorRejectsEveryone covers the second way an empty
// token was reachable, and the one that matters for a package under pkg/.
//
// NewTokenAuthenticator cannot produce this, but TokenAuthenticator is exported
// with unexported fields, so any importer can write &auth.TokenAuthenticator{}
// and get one with no token. Before the guard in UnaryInterceptor, that
// authenticated every caller presenting an empty bearer token.
func TestZeroValueAuthenticatorRejectsEveryone(t *testing.T) {
	a := &TokenAuthenticator{}

	for _, header := range []string{"", "Bearer ", "Bearer anything", "Bearer s3cret"} {
		t.Run("header="+header, func(t *testing.T) {
			reached, code := callWith(t, a, metadata.Pairs("authorization", header))

			if reached {
				t.Error("a zero-value authenticator let a request through")
			}
			if code != codes.Unauthenticated {
				t.Errorf("code = %v, want %v", code, codes.Unauthenticated)
			}
		})
	}
}

// TestReloadKeepsCurrentTokenWhenFileGoesEmpty covers a rotation that is
// observed mid-write. Clearing the in-memory token would be worse than serving
// the previous one, because an empty token used to mean "let everyone in".
func TestReloadKeepsCurrentTokenWhenFileGoesEmpty(t *testing.T) {
	path := writeTokenFile(t, "s3cret")

	a, err := NewTokenAuthenticator(path)
	if err != nil {
		t.Fatalf("constructing authenticator: %v", err)
	}
	t.Cleanup(a.Close)

	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("truncating token file: %v", err)
	}

	// Call loadToken directly rather than waiting for the ticker.
	if err := a.loadToken(); !errors.Is(err, ErrEmptyToken) {
		t.Fatalf("loadToken() err = %v, want %v", err, ErrEmptyToken)
	}

	if a.token != "s3cret" {
		t.Errorf("token = %q, want the previous token to be kept", a.token)
	}

	reached, code := callWith(t, a, metadata.Pairs("authorization", "Bearer s3cret"))
	if !reached || code != codes.OK {
		t.Errorf("the previous token stopped working: reached=%v code=%v", reached, code)
	}

	reached, _ = callWith(t, a, metadata.Pairs("authorization", "Bearer "))
	if reached {
		t.Error("an empty token was accepted after the file was truncated")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	a, err := NewTokenAuthenticator(writeTokenFile(t, "s3cret"))
	if err != nil {
		t.Fatalf("constructing authenticator: %v", err)
	}

	a.Close()
	a.Close() // must not panic on a second close
}
