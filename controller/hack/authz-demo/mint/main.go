// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Command mint makes the bad tokens the authorization demo (hack/authz-demo)
// presents to the NVKE controller. Each command prints one token.
//
//	mint rogue --key rogue-key.pem --iss https://rogue.nvke-demo.local --sub mallory --groups nvke-admins
//	mint sign --key realm-key.pem --jwks certs.json --iss <keycloak issuer> --sub <id> --ttl -10m
//	mint tamper --token <jwt> --claim 'groups=["nvke-admins"]'
//	mint malformed
//
// rogue signs with an ES256 key no configured issuer publishes. sign signs with
// the demo realm's RS256 key and takes the key ID from the realm's JWKS, so a
// negative --ttl makes a token the controller verifies and then refuses as
// expired. tamper rewrites claims and keeps the original signature. malformed
// prints a token with no signature segment.
package main

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "mint:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: mint rogue|sign|tamper|malformed [flags]")
	}
	var (
		token string
		err   error
	)
	switch args[0] {
	case "rogue":
		token, err = rogue(args[1:])
	case "sign":
		token, err = sign(args[1:])
	case "tamper":
		token, err = tamper(args[1:])
	case "malformed":
		token, err = malformed()
	default:
		return fmt.Errorf("unknown command %q: want rogue, sign, tamper or malformed", args[0])
	}
	if err != nil {
		return err
	}
	fmt.Println(token)
	return nil
}

// claimFlags are the claims rogue and sign put in a token.
type claimFlags struct {
	iss, sub, aud, groups string
	ttl                   time.Duration
}

func (c *claimFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&c.iss, "iss", "", "iss (required)")
	fs.StringVar(&c.sub, "sub", "", "sub (required)")
	fs.StringVar(&c.aud, "aud", "nvke-controller", "aud")
	fs.StringVar(&c.groups, "groups", "", "comma-separated groups claim; omitted when empty")
	fs.DurationVar(&c.ttl, "ttl", 5*time.Minute, "lifetime; negative mints an expired token")
}

// signClaims signs c's claims with key under alg, with kid in the header.
func signClaims(alg jose.SignatureAlgorithm, key any, kid string, c *claimFlags) (string, error) {
	if c.iss == "" || c.sub == "" {
		return "", errors.New("--iss and --sub are required")
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: alg, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader(jose.HeaderKey("kid"), kid),
	)
	if err != nil {
		return "", fmt.Errorf("signer: %w", err)
	}
	now := time.Now()
	// An expired token was issued a minute before it expired.
	issued := now
	if c.ttl < 0 {
		issued = now.Add(c.ttl - time.Minute)
	}
	registered := jwt.Claims{
		Issuer:   c.iss,
		Subject:  c.sub,
		Audience: jwt.Audience{c.aud},
		IssuedAt: jwt.NewNumericDate(issued),
		Expiry:   jwt.NewNumericDate(now.Add(c.ttl)),
	}
	private := map[string]any{}
	if c.groups != "" {
		private["groups"] = strings.Split(c.groups, ",")
	}
	return jwt.Signed(signer).Claims(registered).Claims(private).Serialize()
}

func rogue(args []string) (string, error) {
	fs := flag.NewFlagSet("rogue", flag.ContinueOnError)
	keyPath := fs.String("key", "", "PEM ECDSA P-256 private key (required)")
	var c claimFlags
	c.register(fs)
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	key, err := loadKey(*keyPath)
	if err != nil {
		return "", err
	}
	ec, ok := key.(*ecdsa.PrivateKey)
	if !ok || ec.Curve != elliptic.P256() {
		return "", fmt.Errorf("%s is not an ECDSA P-256 key", *keyPath)
	}
	jwk := jose.JSONWebKey{Key: &ec.PublicKey}
	thumb, err := jwk.Thumbprint(crypto.SHA256)
	if err != nil {
		return "", fmt.Errorf("thumbprint: %w", err)
	}
	return signClaims(jose.ES256, ec, base64.RawURLEncoding.EncodeToString(thumb), &c)
}

func sign(args []string) (string, error) {
	fs := flag.NewFlagSet("sign", flag.ContinueOnError)
	keyPath := fs.String("key", "", "PEM RSA private key (required)")
	jwksPath := fs.String("jwks", "", "the issuer's JWKS, to find the key's kid (required)")
	var c claimFlags
	c.register(fs)
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	key, err := loadKey(*keyPath)
	if err != nil {
		return "", err
	}
	rk, ok := key.(*rsa.PrivateKey)
	if !ok {
		return "", fmt.Errorf("%s is not an RSA key", *keyPath)
	}
	kid, err := kidInJWKS(*jwksPath, &rk.PublicKey)
	if err != nil {
		return "", err
	}
	return signClaims(jose.RS256, rk, kid, &c)
}

// kidInJWKS returns the key ID the JWKS at path gives pub.
func kidInJWKS(path string, pub *rsa.PublicKey) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read JWKS: %w", err)
	}
	var set jose.JSONWebKeySet
	if err := json.Unmarshal(raw, &set); err != nil {
		return "", fmt.Errorf("parse JWKS %s: %w", path, err)
	}
	for _, k := range set.Keys {
		if p, ok := k.Key.(*rsa.PublicKey); ok && p.Equal(pub) {
			return k.KeyID, nil
		}
	}
	return "", fmt.Errorf("JWKS %s does not publish the key", path)
}

func tamper(args []string) (string, error) {
	fs := flag.NewFlagSet("tamper", flag.ContinueOnError)
	token := fs.String("token", "", "the token to rewrite (required)")
	changes := map[string]any{}
	fs.Func("claim", "name=<JSON value>; repeatable", func(s string) error {
		name, raw, ok := strings.Cut(s, "=")
		if !ok || name == "" {
			return fmt.Errorf("--claim %q is not name=<JSON value>", s)
		}
		var value any
		if err := json.Unmarshal([]byte(raw), &value); err != nil {
			return fmt.Errorf("--claim %s: %w", name, err)
		}
		changes[name] = value
		return nil
	})
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	parts := strings.Split(*token, ".")
	if len(parts) != 3 {
		return "", errors.New("--token is not a three-part JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("decode payload: %w", err)
	}
	claims := map[string]any{}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("parse payload: %w", err)
	}
	for name, value := range changes {
		claims[name] = value
	}
	payload, err = json.Marshal(claims)
	if err != nil {
		return "", err
	}
	parts[1] = base64.RawURLEncoding.EncodeToString(payload)
	return strings.Join(parts, "."), nil
}

func malformed() (string, error) {
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." +
		enc.EncodeToString([]byte(`{"sub":"nobody"}`)), nil
}

// loadKey reads a PKCS#8, PKCS#1 or SEC 1 PEM private key.
func loadKey(path string) (any, error) {
	if path == "" {
		return nil, errors.New("--key is required")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read key (run hack/authz-demo/gen.sh first): %w", err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("%s holds no PEM block", path)
	}
	switch block.Type {
	case "PRIVATE KEY":
		return x509.ParsePKCS8PrivateKey(block.Bytes)
	case "RSA PRIVATE KEY":
		return x509.ParsePKCS1PrivateKey(block.Bytes)
	case "EC PRIVATE KEY":
		return x509.ParseECPrivateKey(block.Bytes)
	default:
		return nil, fmt.Errorf("%s: unsupported PEM block %q", path, block.Type)
	}
}
