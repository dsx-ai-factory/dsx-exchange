// Copyright 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/MicahParks/jwkset"
	gojwt "github.com/golang-jwt/jwt/v5"
	"github.com/nats-io/jwt/v2"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/synadia-io/callout.go"
	"github.com/uptrace/opentelemetry-go-extra/otelzap"
	"go.uber.org/zap"

	"github.com/NVIDIA/dsx-exchange/auth-callout/src/internal/auth"
	perms "github.com/NVIDIA/dsx-exchange/auth-callout/src/internal/config"
)

func TestRunStopsWhenContextCanceled(t *testing.T) {
	natsServer := runNATSServer(t)
	permissionsFile := filepath.Join(t.TempDir(), "permissions.json")
	require.NoError(t, os.WriteFile(permissionsFile, []byte(`{}`), 0o600))

	issuerKeyPair, err := nkeys.CreateAccount()
	require.NoError(t, err)
	issuerSeed, err := issuerKeyPair.Seed()
	require.NoError(t, err)

	svc := New(ServiceConfig{
		HostConfig: HostConfig{Port: 0},
		NATS: NATSConfig{
			URL:        natsServer.ClientURL(),
			IssuerSeed: string(issuerSeed),
		},
		Permissions: PermissionsFileConfig{File: permissionsFile},
	}, otelzap.New(zap.NewNop()))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() {
		done <- svc.Run(ctx)
	}()

	select {
	case err := <-done:
		require.NoError(t, err)
		require.True(
			t,
			svc.natsConn.IsDraining() || svc.natsConn.IsClosed(),
			"NATS connection was not drained",
		)
	case <-time.After(2 * time.Second):
		t.Fatal("service did not stop after context cancellation")
	}
}

func TestAuthorizationServiceStartsBeforeInitialNATSConnect(t *testing.T) {
	natsServer := runNATSServer(t)
	dialer := &gatedDialer{ready: make(chan struct{})}
	initialConnectCh := make(chan struct{}, 1)

	opts, err := newTestService().buildNATSOptions(initialConnectCh)
	if err != nil {
		t.Fatalf("build NATS options: %v", err)
	}
	opts = append(opts,
		nats.SetCustomDialer(dialer),
		nats.Timeout(50*time.Millisecond),
		nats.ReconnectWait(50*time.Millisecond),
	)

	natsConn, err := nats.Connect(natsServer.ClientURL(), opts...)
	if err != nil {
		t.Fatalf("connect while NATS dial is blocked: %v", err)
	}
	t.Cleanup(natsConn.Close)
	if natsConn.IsConnected() {
		t.Fatal("NATS connection is connected, want reconnecting")
	}
	authService, err := callout.NewAuthorizationService(
		natsConn,
		callout.Name("auth-callout"),
		callout.Authorizer(func(*jwt.AuthorizationRequest) (string, error) {
			return "", nil
		}),
		callout.ResponseSigner(func(*jwt.AuthorizationResponseClaims) (string, error) {
			return "", nil
		}),
		callout.Logger(noopNATSLogger{}),
	)
	if err != nil {
		t.Fatalf("create authorization service while NATS reconnects: %v", err)
	}
	t.Cleanup(func() { _ = authService.Stop() })

	if natsConn.NumSubscriptions() == 0 {
		t.Fatal("authorization service did not register NATS subscriptions")
	}

	close(dialer.ready)
	select {
	case <-initialConnectCh:
	case <-time.After(2 * time.Second):
		t.Fatal("NATS connection did not recover after dialer was unblocked")
	}
	if !natsConn.IsConnected() {
		t.Fatal("NATS connection is not connected after initial connect signal")
	}
}

func TestValidateOAuth2Config(t *testing.T) {
	tests := []struct {
		name             string
		jwksURL          string
		hasOAuth2Entries bool
		wantErr          bool
	}{
		{"oauth2 clients without jwks url", "", true, true},
		{"oauth2 clients with jwks url", "https://idp.example.com/jwks.json", true, false},
		{"jwks url without oauth2 clients", "https://idp.example.com/jwks.json", false, false},
		{"neither configured", "", false, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateOAuth2Config(test.jwksURL, test.hasOAuth2Entries)
			if test.wantErr {
				require.ErrorContains(t, err, "jwks.url")
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestValidateSVIDConfig(t *testing.T) {
	svid := &auth.SVIDConfig{Issuers: []string{"https://svid.example.com"}}
	tests := []struct {
		name           string
		svid           *auth.SVIDConfig
		oauth2Issuer   string
		hasSVIDEntries bool
		wantErr        string
	}{
		{name: "svid entries without svid block", hasSVIDEntries: true, wantErr: "svid block"},
		{name: "svid entries with svid block", svid: svid, hasSVIDEntries: true},
		{name: "svid block without svid entries", svid: svid},
		{name: "neither configured"},
		{name: "different oauth2 issuer", svid: svid, oauth2Issuer: "https://oauth2.example.com"},
		{name: "shared issuer", svid: svid, oauth2Issuer: "https://svid.example.com", wantErr: "also the jwks issuer"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateSVIDConfig(test.svid, test.oauth2Issuer, test.hasSVIDEntries)
			if test.wantErr != "" {
				require.ErrorContains(t, err, test.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}

// TestSVIDConnectionsThroughAuthCallout runs the service as the auth callout of
// a real NATS server and connects clients with JWT-SVIDs.
func TestSVIDConnectionsThroughAuthCallout(t *testing.T) {
	const (
		issuer       = "https://issuer.example.com"
		oauth2Issuer = "https://oauth2.example.com"
	)
	signingKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	calloutUser, err := nkeys.CreateUser()
	require.NoError(t, err)
	calloutUserSeed, err := calloutUser.Seed()
	require.NoError(t, err)
	calloutUserPub, err := calloutUser.PublicKey()
	require.NoError(t, err)
	issuerAccount, err := nkeys.CreateAccount()
	require.NoError(t, err)
	issuerSeed, err := issuerAccount.Seed()
	require.NoError(t, err)
	issuerPub, err := issuerAccount.PublicKey()
	require.NoError(t, err)

	natsServer := runAuthCalloutNATSServer(t, issuerPub, calloutUserPub)

	// noauth grants everything, so a refused SVID proves it never falls through.
	permissions, err := json.Marshal(perms.PermissionsConfig{
		SVID: map[string]*perms.SVIDEntry{
			"svid-test": {
				SVIDKey: perms.SVIDKey{Tenant: "tenant-1", Region: "region-1", Service: "dsx", Partition: "example.com", Role: "svid-test"},
				Account: "CSC",
				Permissions: jwt.Permissions{
					Pub: jwt.Permission{Allow: []string{"svid.test.>"}},
					Sub: jwt.Permission{Allow: []string{"svid.test.>"}},
				},
			},
		},
		OAuth2: map[string]*perms.OAuth2Entry{
			"oauth2-test": {
				Azp:     "oauth2-client",
				Account: "CSC",
				Permissions: jwt.Permissions{
					Pub: jwt.Permission{Allow: []string{"oauth2.test.>"}},
					Sub: jwt.Permission{Allow: []string{"oauth2.test.>"}},
				},
			},
		},
		NoAuth: &perms.NoAuthEntry{Account: "CSC"},
	})
	require.NoError(t, err)
	permissionsFile := filepath.Join(t.TempDir(), "permissions.json")
	require.NoError(t, os.WriteFile(permissionsFile, permissions, 0o600))

	// OAuth2 shares the key set with SVID, so only the issuer decides which
	// authenticator gets a token.
	jwksURL := serveSigningKey(t, "svid-key", signingKey)
	svc := New(ServiceConfig{
		HostConfig: HostConfig{Port: 0},
		NATS: NATSConfig{
			URL:        natsServer.ClientURL(),
			NKeySeed:   string(calloutUserSeed),
			IssuerSeed: string(issuerSeed),
		},
		JWKS: JWKSConfig{
			URL:               jwksURL,
			Issuer:            oauth2Issuer,
			Audience:          "dsx-exchange",
			SigningAlgorithms: []string{"ES256"},
			AllowInsecure:     true,
		},
		SVID: &auth.SVIDConfig{
			JWKSURL:           jwksURL,
			AllowInsecure:     true,
			Issuers:           []string{issuer},
			Audience:          "dsx-exchange",
			SigningAlgorithms: []string{"ES256"},
		},
		Permissions: PermissionsFileConfig{File: permissionsFile},
	}, otelzap.New(zap.NewNop()))
	runService(t, svc)

	authAccount, err := natsServer.LookupAccount("AUTH")
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return authAccount.SubscriptionInterest(natsserver.AuthCalloutSubject)
	}, 5*time.Second, 20*time.Millisecond, "auth callout service did not subscribe")

	sign := func(claims gojwt.MapClaims) string {
		jwtToken := gojwt.NewWithClaims(gojwt.SigningMethodES256, claims)
		jwtToken.Header["kid"] = "svid-key"
		signed, err := jwtToken.SignedString(signingKey)
		require.NoError(t, err)
		return signed
	}
	tokenExpiringAt := func(role string, expiresAt time.Time) string {
		claims := gojwt.MapClaims{
			"iss":                      issuer,
			"sub":                      "spiffe://tenant-1.region-1.dsx.example.com/workload-" + role,
			"aud":                      []string{"dsx-exchange"},
			"exp":                      expiresAt.Unix(),
			"dsx.nvidia.com/tenant":    "tenant-1",
			"dsx.nvidia.com/region":    "region-1",
			"dsx.nvidia.com/service":   "dsx",
			"dsx.nvidia.com/partition": "example.com",
			"dsx.nvidia.com/resource": []map[string]string{
				{"resource_type": "role", "resource_id": role},
			},
		}
		return sign(claims)
	}
	token := func(role string) string {
		return tokenExpiringAt(role, time.Now().Add(5*time.Minute))
	}

	t.Run("NATS client with username svid", func(t *testing.T) {
		nc, _ := connectWithAsyncErrors(t, natsServer.ClientURL(), nats.UserInfo("svid", token("svid-test")))

		sub, err := nc.SubscribeSync("svid.test.hello")
		require.NoError(t, err)
		require.NoError(t, nc.Publish("svid.test.hello", []byte("hi")))
		msg, err := sub.NextMsg(2 * time.Second)
		require.NoError(t, err)
		assert.Equal(t, "hi", string(msg.Data))
	})

	// OAuth2 is on too, so this also shows that SVID claims its own tokens.
	t.Run("NATS client with a token", func(t *testing.T) {
		nc, _ := connectWithAsyncErrors(t, natsServer.ClientURL(), nats.Token(token("svid-test")))

		sub, err := nc.SubscribeSync("svid.test.token")
		require.NoError(t, err)
		require.NoError(t, nc.Publish("svid.test.token", []byte("hi")))
		msg, err := sub.NextMsg(2 * time.Second)
		require.NoError(t, err)
		assert.Equal(t, "hi", string(msg.Data))
	})

	t.Run("OAuth2 token still reaches OAuth2", func(t *testing.T) {
		oauth2Token := sign(gojwt.MapClaims{
			"iss":   oauth2Issuer,
			"sub":   "oauth2-client",
			"azp":   "oauth2-client",
			"aud":   []string{"dsx-exchange"},
			"exp":   time.Now().Add(5 * time.Minute).Unix(),
			"scope": "mqtt",
		})
		nc, _ := connectWithAsyncErrors(t, natsServer.ClientURL(), nats.Token(oauth2Token))

		sub, err := nc.SubscribeSync("oauth2.test.hello")
		require.NoError(t, err)
		require.NoError(t, nc.Publish("oauth2.test.hello", []byte("hi")))
		msg, err := sub.NextMsg(2 * time.Second)
		require.NoError(t, err)
		assert.Equal(t, "hi", string(msg.Data))
	})

	t.Run("MQTT client publishes to an allowed topic", func(t *testing.T) {
		nc, _ := connectWithAsyncErrors(t, natsServer.ClientURL(), nats.UserInfo("svid", token("svid-test")))
		sub, err := nc.SubscribeSync("svid.test.mqtt")
		require.NoError(t, err)
		require.NoError(t, nc.Flush())

		conn, rc := mqttConnect(t, mqttAddr(t, natsServer), "svid-mqtt", "svid", token("svid-test"))
		require.Equal(t, byte(0), rc, "CONNACK return code")
		writeMQTTPacket(t, conn, 0x30, append(mqttString("svid/test/mqtt"), "hi"...)) // PUBLISH, QoS 0

		msg, err := sub.NextMsg(2 * time.Second)
		require.NoError(t, err)
		assert.Equal(t, "hi", string(msg.Data))
	})

	t.Run("MQTT client with an invalid token is refused", func(t *testing.T) {
		_, rc := mqttConnect(t, mqttAddr(t, natsServer), "svid-mqtt-invalid", "svid", "not-a-jwt")
		assert.NotEqual(t, byte(0), rc, "CONNACK return code")
	})

	t.Run("publish outside allowed subjects is denied", func(t *testing.T) {
		nc, asyncErrors := connectWithAsyncErrors(t, natsServer.ClientURL(), nats.UserInfo("svid", token("svid-test")))

		require.NoError(t, nc.Publish("other.hello", []byte("hi")))
		require.NoError(t, nc.Flush())
		select {
		case err := <-asyncErrors:
			assert.ErrorContains(t, err, `Permissions Violation for Publish to "other.hello"`)
		case <-time.After(2 * time.Second):
			t.Fatal("publish to other.hello was not denied")
		}
	})

	// Credentials are checked only at connect: the user JWT carries no expiry,
	// so the connection outlives the SVID and only new connections need a
	// fresh one.
	t.Run("connection outlives its token", func(t *testing.T) {
		// exp has whole-second precision. Keep it past the 2s connect timeout.
		expiresAt := time.Now().Truncate(time.Second).Add(3 * time.Second)
		shortLived := tokenExpiringAt("svid-test", expiresAt)
		nc, asyncErrors := connectWithAsyncErrors(t, natsServer.ClientURL(),
			nats.UserInfo("svid", shortLived), nats.NoReconnect())
		sub, err := nc.SubscribeSync("svid.test.after-expiry")
		require.NoError(t, err)

		// The condition under test is the wall clock passing exp, so wait for it.
		<-time.After(time.Until(expiresAt) + time.Second)

		require.NoError(t, nc.Publish("svid.test.after-expiry", []byte("still connected")))
		msg, err := sub.NextMsg(2 * time.Second)
		require.NoError(t, err)
		assert.Equal(t, "still connected", string(msg.Data))
		assert.True(t, nc.IsConnected())
		assert.Empty(t, asyncErrors)

		reconnect, err := nats.Connect(natsServer.ClientURL(), nats.UserInfo("svid", shortLived), nats.Timeout(2*time.Second))
		if reconnect != nil {
			reconnect.Close()
		}
		require.ErrorIs(t, err, nats.ErrAuthorization)
	})

	t.Run("anonymous client still gets noauth", func(t *testing.T) {
		nc, err := nats.Connect(natsServer.ClientURL(), nats.Timeout(2*time.Second))
		require.NoError(t, err)
		nc.Close()
	})

	t.Run("role without a rule is refused", func(t *testing.T) {
		nc, err := nats.Connect(natsServer.ClientURL(), nats.UserInfo("svid", token("no-rule")), nats.Timeout(2*time.Second))
		if nc != nil {
			nc.Close()
		}
		require.ErrorIs(t, err, nats.ErrAuthorization)
	})

	t.Run("svid username without a token is refused", func(t *testing.T) {
		nc, err := nats.Connect(natsServer.ClientURL(), nats.UserInfo("svid", ""), nats.Timeout(2*time.Second))
		if nc != nil {
			nc.Close()
		}
		require.ErrorIs(t, err, nats.ErrAuthorization)
	})

	t.Run("invalid token is refused", func(t *testing.T) {
		nc, err := nats.Connect(natsServer.ClientURL(), nats.UserInfo("svid", "not-a-jwt"), nats.Timeout(2*time.Second))
		if nc != nil {
			nc.Close()
		}
		require.ErrorIs(t, err, nats.ErrAuthorization)
	})
}

// runAuthCalloutNATSServer starts a NATS server with an MQTT listener that
// delegates every connection, except the callout user's, to the auth callout
// service. MQTT needs JetStream in the account its clients use.
func runAuthCalloutNATSServer(t *testing.T, issuerPub, calloutUserPub string) *natsserver.Server {
	t.Helper()

	configFile := filepath.Join(t.TempDir(), "nats.conf")
	config := fmt.Sprintf(`
listen: "127.0.0.1:-1"
server_name: "auth-callout-test"
jetstream { store_dir: %q }
mqtt { listen: "127.0.0.1:-1" }
accounts {
  AUTH { users: [ { nkey: %q } ] }
  CSC { jetstream: enabled }
}
authorization {
  auth_callout {
    issuer: %q
    account: AUTH
    auth_users: [ %q ]
  }
}
`, t.TempDir(), calloutUserPub, issuerPub, calloutUserPub)
	require.NoError(t, os.WriteFile(configFile, []byte(config), 0o600))

	opts, err := natsserver.ProcessConfigFile(configFile)
	require.NoError(t, err)
	opts.NoLog = true
	opts.NoSigs = true

	natsServer, err := natsserver.NewServer(opts)
	require.NoError(t, err)
	natsServer.Start()
	t.Cleanup(natsServer.Shutdown)
	require.True(t, natsServer.ReadyForConnections(2*time.Second), "NATS server did not become ready")
	return natsServer
}

// runService runs svc until the test ends.
func runService(t *testing.T, svc *Service) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- svc.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Error("service did not stop")
		}
	})
}

// serveSigningKey serves the public half of key as a JWKS until the test ends.
func serveSigningKey(t *testing.T, kid string, key *ecdsa.PrivateKey) string {
	t.Helper()

	storage := jwkset.NewMemoryStorage()
	jwk, err := jwkset.NewJWKFromKey(key, jwkset.JWKOptions{Metadata: jwkset.JWKMetadataOptions{KID: kid}})
	require.NoError(t, err)
	require.NoError(t, storage.KeyWrite(context.Background(), jwk))

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

// mqttAddr returns the address of the server's MQTT listener.
func mqttAddr(t *testing.T, natsServer *natsserver.Server) string {
	t.Helper()

	varz, err := natsServer.Varz(nil)
	require.NoError(t, err)
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(varz.MQTT.Port))
}

// mqttConnect sends an MQTT 3.1.1 CONNECT with a username and password and
// returns the connection and the CONNACK return code.
func mqttConnect(t *testing.T, addr, clientID, username, password string) (net.Conn, byte) {
	t.Helper()

	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))

	// Protocol "MQTT" level 4, flags username, password and clean session, keep alive 60s.
	body := []byte{0, 4, 'M', 'Q', 'T', 'T', 4, 0xC2, 0, 60}
	for _, field := range []string{clientID, username, password} {
		body = append(body, mqttString(field)...)
	}
	writeMQTTPacket(t, conn, 0x10, body) // CONNECT

	connack := make([]byte, 4)
	_, err = io.ReadFull(conn, connack)
	require.NoError(t, err)
	require.Equal(t, byte(0x20), connack[0], "expected CONNACK")
	return conn, connack[3]
}

// writeMQTTPacket writes a packet with the given first header byte.
func writeMQTTPacket(t *testing.T, conn net.Conn, header byte, body []byte) {
	t.Helper()

	packet := []byte{header}
	for n := len(body); ; {
		digit := byte(n % 128)
		n /= 128
		if n > 0 {
			digit |= 0x80
		}
		packet = append(packet, digit)
		if n == 0 {
			break
		}
	}
	_, err := conn.Write(append(packet, body...))
	require.NoError(t, err)
}

// mqttString encodes s as an MQTT UTF-8 string: a two-byte length, then s.
func mqttString(s string) []byte {
	return append([]byte{byte(len(s) >> 8), byte(len(s))}, s...)
}

// connectWithAsyncErrors connects to url and returns the server errors, such
// as permission violations, that arrive after the connection is up.
func connectWithAsyncErrors(t *testing.T, url string, opts ...nats.Option) (*nats.Conn, <-chan error) {
	t.Helper()

	asyncErrors := make(chan error, 8)
	opts = append(opts,
		nats.Timeout(2*time.Second),
		nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
			asyncErrors <- err
		}),
	)
	nc, err := nats.Connect(url, opts...)
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	return nc, asyncErrors
}

func TestLivezHandlerDoesNotRequireNATSConnection(t *testing.T) {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/livez", nil)

	livezHandler(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if body := response.Body.String(); body != "OK" {
		t.Fatalf("body = %q, want %q", body, "OK")
	}
}

func TestHealthHandlerRequiresNATSConnection(t *testing.T) {
	natsConn := newReconnectingNATSConn(t)

	service := &Service{natsConn: natsConn}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)

	service.HealthHandler(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
}

func newReconnectingNATSConn(t *testing.T) *nats.Conn {
	t.Helper()

	opts, err := newTestService().buildNATSOptions(make(chan struct{}, 1))
	if err != nil {
		t.Fatalf("build NATS options: %v", err)
	}
	opts = append(opts,
		nats.SetCustomDialer(failingDialer{}),
		nats.Timeout(50*time.Millisecond),
		nats.ReconnectWait(50*time.Millisecond),
	)

	natsConn, err := nats.Connect("nats://unavailable.test:4222", opts...)
	if err != nil {
		t.Fatalf("connect to unavailable NATS with retry enabled: %v", err)
	}
	t.Cleanup(natsConn.Close)

	return natsConn
}

func runNATSServer(t *testing.T) *natsserver.Server {
	t.Helper()

	natsServer, err := natsserver.NewServer(&natsserver.Options{
		Host:   "127.0.0.1",
		Port:   -1,
		NoLog:  true,
		NoSigs: true,
	})
	if err != nil {
		t.Fatalf("create NATS server: %v", err)
	}
	natsServer.Start()
	if !natsServer.ReadyForConnections(2 * time.Second) {
		natsServer.Shutdown()
		t.Fatal("NATS server did not become ready")
	}
	t.Cleanup(natsServer.Shutdown)

	return natsServer
}

func newTestService() *Service {
	return &Service{
		logger: otelzap.New(zap.NewNop()),
	}
}

type failingDialer struct{}

func (failingDialer) Dial(string, string) (net.Conn, error) {
	return nil, errors.New("dial failed")
}

type gatedDialer struct {
	ready chan struct{}
}

func (d *gatedDialer) Dial(network, address string) (net.Conn, error) {
	select {
	case <-d.ready:
		return (&net.Dialer{}).Dial(network, address)
	default:
		return nil, errors.New("dial blocked")
	}
}

type noopNATSLogger struct{}

func (noopNATSLogger) Noticef(string, ...any) {}
func (noopNATSLogger) Warnf(string, ...any)   {}
func (noopNATSLogger) Fatalf(string, ...any)  {}
func (noopNATSLogger) Errorf(string, ...any)  {}
func (noopNATSLogger) Debugf(string, ...any)  {}
func (noopNATSLogger) Tracef(string, ...any)  {}
