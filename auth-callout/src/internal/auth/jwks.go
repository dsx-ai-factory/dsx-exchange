// Copyright 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
)

// newJWKS returns a key set fetched from jwksURL that refreshes in the
// background until ctx is canceled. Plain HTTP, including a redirect to it, is
// refused unless allowInsecure is set, and an HTTPS URL never redirects to HTTP.
func newJWKS(ctx context.Context, jwksURL string, allowInsecure bool) (keyfunc.Keyfunc, error) {
	if err := validateJWKSURL(jwksURL, allowInsecure); err != nil {
		return nil, err
	}

	client := *http.DefaultClient
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return validateJWKSRedirect(req, via, allowInsecure)
	}

	k, err := keyfunc.NewDefaultOverrideCtx(ctx, []string{jwksURL}, keyfunc.Override{Client: &client})
	if err != nil {
		return nil, fmt.Errorf("failed to create JWKS client: %w", err)
	}
	return k, nil
}

func validateJWKSURL(rawURL string, allowInsecure bool) error {
	parsedURL, err := url.ParseRequestURI(rawURL)
	if err != nil {
		return fmt.Errorf("invalid JWKS URL: %w", err)
	}
	if parsedURL.Host == "" {
		return fmt.Errorf("invalid JWKS URL: host is required")
	}

	switch parsedURL.Scheme {
	case "https":
		return nil
	case "http":
		if allowInsecure {
			return nil
		}
		return fmt.Errorf("JWKS URL must use HTTPS")
	default:
		return fmt.Errorf("JWKS URL must use HTTP or HTTPS")
	}
}

func validateJWKSRedirect(req *http.Request, via []*http.Request, allowInsecure bool) error {
	if len(via) >= 10 {
		return fmt.Errorf("stopped after 10 JWKS redirects")
	}
	if len(via) > 0 && via[len(via)-1].URL.Scheme == "https" && req.URL.Scheme != "https" {
		return fmt.Errorf("JWKS redirects must not downgrade HTTPS to HTTP")
	}

	return validateJWKSURL(req.URL.String(), allowInsecure)
}

// validateSigningAlgorithms checks that every algorithm configured for method,
// such as OAuth2, verifies with a public key from a JWKS.
func validateSigningAlgorithms(method string, configured []string) error {
	if len(configured) == 0 {
		return fmt.Errorf("%s signing algorithms are required", method)
	}

	for _, algorithm := range configured {
		if !isPublicKeyAlgorithm(algorithm) {
			return fmt.Errorf("unsupported %s signing algorithm %q", method, algorithm)
		}
	}
	return nil
}

// isPublicKeyAlgorithm reports whether alg names an RSA, RSA-PSS, ECDSA or
// EdDSA signing method. HMAC, none and unknown names are not.
func isPublicKeyAlgorithm(alg string) bool {
	switch jwt.GetSigningMethod(alg).(type) {
	case *jwt.SigningMethodRSA, *jwt.SigningMethodRSAPSS, *jwt.SigningMethodECDSA, *jwt.SigningMethodEd25519:
		return true
	default:
		return false
	}
}
