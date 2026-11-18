// Copyright 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MicahParks/jwkset"
	gojwt "github.com/golang-jwt/jwt/v5"
	"github.com/nats-io/jwt/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/NVIDIA/dsx-exchange/auth-callout/src/internal/config"
)

const (
	testSVIDIssuer   = "https://issuer.example.com"
	testSVIDAudience = "dsx-exchange"
	testSVIDKID      = "svid-key"
	testRSAKID       = "rsa-key"
	testSVIDSubject  = "spiffe://tenant-1.region-1.dsx.example.com/workload-svid-test"
)

// svidFixture is a JWKS server with one ES256 and one RS256 key, and an SVID
// authenticator that trusts only ES256.
type svidFixture struct {
	auth   *SVIDAuthenticator
	ecKey  *ecdsa.PrivateKey
	rsaKey *rsa.PrivateKey
}

func newSVIDFixture(t *testing.T) svidFixture {
	t.Helper()

	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	jwksURL := serveJWKS(t, map[string]any{testSVIDKID: ecKey, testRSAKID: rsaKey})

	pm, err := config.NewPermissionsManager(writeSVIDPermissions(t), testLogger())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pm.Close()) })

	svidAuth, err := NewSVIDAuthenticator(SVIDConfig{
		JWKSURL:           jwksURL,
		AllowInsecure:     true,
		Issuers:           []string{testSVIDIssuer},
		Audience:          testSVIDAudience,
		SigningAlgorithms: []string{gojwt.SigningMethodES256.Alg()},
	}, pm, testLogger(), testServiceName)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, svidAuth.Close()) })

	return svidFixture{auth: svidAuth, ecKey: ecKey, rsaKey: rsaKey}
}

func TestSVIDTryAuthenticate(t *testing.T) {
	fixture := newSVIDFixture(t)
	otherKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tests := []struct {
		name string
		// edit changes the claims of a valid token for role svid-test.
		edit func(claims gojwt.MapClaims)
		// token signs the claims; nil signs them with the trusted ES256 key.
		token     func(t *testing.T, claims gojwt.MapClaims) string
		wantPub   jwt.StringList
		wantError string
	}{
		{
			name:    "role rule",
			wantPub: jwt.StringList{"svid.test.>"},
		},
		{
			name: "role without a rule",
			edit: func(claims gojwt.MapClaims) {
				claims["dsx.nvidia.com/resource"] = resources("role", "other")
			},
			wantError: "no SVID rule",
		},
		{
			name: "rule without a role matches a token without a role",
			edit: func(claims gojwt.MapClaims) {
				// Entries of other types are not a role.
				claims["dsx.nvidia.com/resource"] = resources("host", "host-1")
			},
			wantPub: jwt.StringList{"svid.any.>"},
		},
		{
			name:      "no rule for the service",
			edit:      func(claims gojwt.MapClaims) { claims["dsx.nvidia.com/service"] = "other" },
			wantError: "no SVID rule matches " + testSVIDSubject,
		},
		{
			name:      "no rule for the region",
			edit:      func(claims gojwt.MapClaims) { claims["dsx.nvidia.com/region"] = "region-2" },
			wantError: "no SVID rule matches",
		},
		{
			name: "bad signature",
			token: func(t *testing.T, claims gojwt.MapClaims) string {
				return signToken(t, gojwt.SigningMethodES256, otherKey, testSVIDKID, claims)
			},
			wantError: "signature is invalid",
		},
		{
			name: "algorithm not configured",
			token: func(t *testing.T, claims gojwt.MapClaims) string {
				return signToken(t, gojwt.SigningMethodRS256, fixture.rsaKey, testRSAKID, claims)
			},
			wantError: "signing method RS256 is invalid",
		},
		{
			name:      "untrusted issuer",
			edit:      func(claims gojwt.MapClaims) { claims["iss"] = "https://other-issuer.example.com" },
			wantError: `SVID issuer "https://other-issuer.example.com" is not trusted`,
		},
		{
			name:      "wrong audience",
			edit:      func(claims gojwt.MapClaims) { claims["aud"] = []string{"spire"} },
			wantError: "token has invalid audience",
		},
		{
			name:      "expired",
			edit:      func(claims gojwt.MapClaims) { claims["exp"] = time.Now().Add(-time.Minute).Unix() },
			wantError: "token is expired",
		},
		{
			name:      "no expiry",
			edit:      func(claims gojwt.MapClaims) { delete(claims, "exp") },
			wantError: "exp claim is required",
		},
		{
			name:      "missing tenant",
			edit:      func(claims gojwt.MapClaims) { delete(claims, "dsx.nvidia.com/tenant") },
			wantError: "SVID must carry sub, tenant, region, service and partition claims",
		},
		{
			name: "two roles",
			edit: func(claims gojwt.MapClaims) {
				claims["dsx.nvidia.com/resource"] = resources("role", "svid-test", "role", "other")
			},
			wantError: "carries more than one role",
		},
		{
			name: "empty role",
			edit: func(claims gojwt.MapClaims) {
				claims["dsx.nvidia.com/resource"] = resources("role", "")
			},
			wantError: "carries an empty role",
		},
		{
			name: "not a JWT",
			token: func(*testing.T, gojwt.MapClaims) string {
				return "not-a-jwt"
			},
			wantError: "token is malformed",
		},
		{
			name: "empty token",
			token: func(*testing.T, gojwt.MapClaims) string {
				return ""
			},
			wantError: "SVID connection has no token",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claims := validSVIDClaims()
			if tt.edit != nil {
				tt.edit(claims)
			}
			token := signToken(t, gojwt.SigningMethodES256, fixture.ecKey, testSVIDKID, claims)
			if tt.token != nil {
				token = tt.token(t, claims)
			}

			profile, err := fixture.auth.TryAuthenticate(context.Background(), svidRequest(token))
			if tt.wantError != "" {
				require.ErrorContains(t, err, tt.wantError)
				assert.Equal(t, config.UserProfile{}, profile)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, testSVIDSubject, profile.Name)
			assert.Equal(t, "CSC", profile.Account)
			assert.Equal(t, tt.wantPub, profile.Permissions.Pub.Allow)
			assert.Equal(t, jwt.Permission{}, profile.Permissions.Sub, "omitted sub keeps the NATS default")
		})
	}
}

func TestSVIDCanAuthenticate(t *testing.T) {
	fixture := newSVIDFixture(t)
	trusted := signToken(t, gojwt.SigningMethodES256, fixture.ecKey, testSVIDKID, validSVIDClaims())
	otherIssuer := validSVIDClaims()
	otherIssuer["iss"] = "https://other.example.com"
	untrusted := signToken(t, gojwt.SigningMethodES256, fixture.ecKey, testSVIDKID, otherIssuer)

	tests := []struct {
		name     string
		username string
		password string
		token    string
		want     bool
	}{
		{name: "svid username with token", username: "svid", password: "a.b.c", want: true},
		{name: "svid username without token", username: "svid", want: true},
		{name: "oauth2 username", username: "oauthtoken", password: "a.b.c"},
		{name: "nats token from a trusted issuer", token: trusted, want: true},
		{name: "nats token from another issuer", token: untrusted},
		{name: "nats token that is not a JWT", token: "a.b.c"},
		{name: "no credentials"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rc := &jwt.AuthorizationRequestClaims{}
			rc.ConnectOptions.Username = tt.username
			rc.ConnectOptions.Password = tt.password
			rc.ConnectOptions.Token = tt.token

			assert.Equal(t, tt.want, fixture.auth.CanAuthenticate(rc))
		})
	}
}

func TestSVIDTryAuthenticateNATSToken(t *testing.T) {
	fixture := newSVIDFixture(t)
	rc := &jwt.AuthorizationRequestClaims{}
	rc.ConnectOptions.Token = signToken(t, gojwt.SigningMethodES256, fixture.ecKey, testSVIDKID, validSVIDClaims())

	profile, err := fixture.auth.TryAuthenticate(context.Background(), rc)
	require.NoError(t, err)
	assert.Equal(t, testSVIDSubject, profile.Name)
	assert.Equal(t, jwt.StringList{"svid.test.>"}, profile.Permissions.Pub.Allow)
}

func TestNewSVIDAuthenticatorRejectsInvalidConfig(t *testing.T) {
	pm, err := config.NewPermissionsManager(writeSVIDPermissions(t), testLogger())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pm.Close()) })

	// Every case breaks one field of a config that is otherwise valid.
	valid := SVIDConfig{
		JWKSURL:           "https://keys.example.com/jwks.json",
		Issuers:           []string{testSVIDIssuer},
		Audience:          testSVIDAudience,
		SigningAlgorithms: []string{"ES256"},
	}

	tests := []struct {
		name      string
		edit      func(cfg *SVIDConfig)
		wantError string
	}{
		{
			name:      "no issuers",
			edit:      func(cfg *SVIDConfig) { cfg.Issuers = nil },
			wantError: "SVID issuers are required",
		},
		{
			name:      "empty issuer",
			edit:      func(cfg *SVIDConfig) { cfg.Issuers = []string{testSVIDIssuer, ""} },
			wantError: "SVID issuers must not be empty",
		},
		{
			name:      "no audience",
			edit:      func(cfg *SVIDConfig) { cfg.Audience = "" },
			wantError: "SVID audience is required",
		},
		{
			name:      "no signing algorithms",
			edit:      func(cfg *SVIDConfig) { cfg.SigningAlgorithms = nil },
			wantError: "SVID signing algorithms are required",
		},
		{
			name:      "HMAC signing algorithm",
			edit:      func(cfg *SVIDConfig) { cfg.SigningAlgorithms = []string{"HS256"} },
			wantError: `unsupported SVID signing algorithm "HS256"`,
		},
		{
			name:      "none signing algorithm",
			edit:      func(cfg *SVIDConfig) { cfg.SigningAlgorithms = []string{"none"} },
			wantError: `unsupported SVID signing algorithm "none"`,
		},
		{
			name:      "plain HTTP JWKS without allow-insecure",
			edit:      func(cfg *SVIDConfig) { cfg.JWKSURL = "http://keys.example.com/jwks.json" },
			wantError: "JWKS URL must use HTTPS",
		},
		{
			name:      "missing JWKS URL",
			edit:      func(cfg *SVIDConfig) { cfg.JWKSURL = "" },
			wantError: "invalid JWKS URL",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid
			tt.edit(&cfg)

			svidAuth, err := NewSVIDAuthenticator(cfg, pm, testLogger(), testServiceName)
			require.ErrorContains(t, err, tt.wantError)
			assert.Nil(t, svidAuth)
		})
	}
}

func TestSVIDRecordsAttemptsAndFailureReasons(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	previous := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(previous) })

	fixture := newSVIDFixture(t)
	valid := validSVIDClaims()
	noRule := validSVIDClaims()
	noRule["dsx.nvidia.com/service"] = "other"

	for _, claims := range []gojwt.MapClaims{valid, noRule} {
		token := signToken(t, gojwt.SigningMethodES256, fixture.ecKey, testSVIDKID, claims)
		_, _ = fixture.auth.TryAuthenticate(context.Background(), svidRequest(token))
	}
	_, _ = fixture.auth.TryAuthenticate(context.Background(), svidRequest(""))

	var collected metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &collected))
	assert.Equal(t, map[string]int64{"": 3}, counterTotals(t, collected, "auth_svid_attempts_total"))
	assert.Equal(t, map[string]int64{
		string(svidReasonNoRule):       1,
		string(svidReasonMissingToken): 1,
	}, counterTotals(t, collected, "auth_svid_failures_total"))
}

// counterTotals returns a counter's values by their reason attribute.
func counterTotals(t *testing.T, collected metricdata.ResourceMetrics, name string) map[string]int64 {
	t.Helper()

	totals := map[string]int64{}
	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok, "%s is not an int64 counter", name)
			for _, point := range sum.DataPoints {
				reason, _ := point.Attributes.Value("reason")
				totals[reason.AsString()] += point.Value
			}
		}
	}
	return totals
}

func validSVIDClaims() gojwt.MapClaims {
	return gojwt.MapClaims{
		"iss":                      testSVIDIssuer,
		"sub":                      testSVIDSubject,
		"aud":                      []string{testSVIDAudience},
		"exp":                      time.Now().Add(5 * time.Minute).Unix(),
		"iat":                      time.Now().Unix(),
		"dsx.nvidia.com/tenant":    "tenant-1",
		"dsx.nvidia.com/region":    "region-1",
		"dsx.nvidia.com/service":   "dsx",
		"dsx.nvidia.com/partition": "example.com",
		"dsx.nvidia.com/resource":  resources("role", "svid-test"),
	}
}

// resources builds a dsx.nvidia.com/resource claim from type and ID pairs.
func resources(typeAndID ...string) []map[string]string {
	var claim []map[string]string
	for i := 0; i+1 < len(typeAndID); i += 2 {
		claim = append(claim, map[string]string{"resource_type": typeAndID[i], "resource_id": typeAndID[i+1]})
	}
	return claim
}

func svidRequest(token string) *jwt.AuthorizationRequestClaims {
	rc := &jwt.AuthorizationRequestClaims{}
	rc.ConnectOptions.Username = "svid"
	rc.ConnectOptions.Password = token
	return rc
}

func signToken(t *testing.T, method gojwt.SigningMethod, key crypto.Signer, kid string, claims gojwt.MapClaims) string {
	t.Helper()

	token := gojwt.NewWithClaims(method, claims)
	token.Header["kid"] = kid
	signed, err := token.SignedString(key)
	require.NoError(t, err)
	return signed
}

// serveJWKS serves the public halves of keys, by key ID, until the test ends.
func serveJWKS(t *testing.T, keys map[string]any) string {
	t.Helper()

	storage := jwkset.NewMemoryStorage()
	for kid, key := range keys {
		jwk, err := jwkset.NewJWKFromKey(key, jwkset.JWKOptions{Metadata: jwkset.JWKMetadataOptions{KID: kid}})
		require.NoError(t, err)
		require.NoError(t, storage.KeyWrite(context.Background(), jwk))
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		document, err := storage.JSONPublic(r.Context())
		if err != nil {
			http.Error(w, "failed to build JWKS", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(document)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// writeSVIDPermissions writes a rule for role svid-test of service dsx and a
// rule for the same service without a role.
func writeSVIDPermissions(t *testing.T) string {
	t.Helper()

	data, err := json.Marshal(config.PermissionsConfig{
		SVID: map[string]*config.SVIDEntry{
			"dsx-svid-test": {
				SVIDKey:     config.SVIDKey{Tenant: "tenant-1", Region: "region-1", Service: "dsx", Partition: "example.com", Role: "svid-test"},
				Account:     "CSC",
				Permissions: jwt.Permissions{Pub: jwt.Permission{Allow: []string{"svid.test.>"}}},
			},
			"dsx-any-role": {
				SVIDKey:     config.SVIDKey{Tenant: "tenant-1", Region: "region-1", Service: "dsx", Partition: "example.com"},
				Account:     "CSC",
				Permissions: jwt.Permissions{Pub: jwt.Permission{Allow: []string{"svid.any.>"}}},
			},
		},
	})
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "permissions.json")
	require.NoError(t, os.WriteFile(path, data, 0o600))
	return path
}
