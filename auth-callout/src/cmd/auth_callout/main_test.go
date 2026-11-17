// Copyright 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nats-io/nkeys"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/dsx-exchange/auth-callout/src/internal/appconfig"
	"github.com/NVIDIA/dsx-exchange/auth-callout/src/internal/auth"
	"github.com/NVIDIA/dsx-exchange/auth-callout/src/internal/service"
)

func TestRunServerDoesNotLogConfiguredNATSSeeds(t *testing.T) {
	permissionsFile := t.TempDir() + "/permissions.json"
	require.NoError(t, os.WriteFile(permissionsFile, []byte(`{}`), 0o600))

	natsKey := mustSeed(t, nkeys.CreateUser)
	issuerKey := mustSeed(t, nkeys.CreateAccount)
	xKey := mustSeed(t, nkeys.CreateCurveKeys)

	t.Setenv("AUTH_CALLOUT_NATS_NKEY_SEED", natsKey)
	t.Setenv("AUTH_CALLOUT_NATS_ISSUER_SEED", issuerKey)
	t.Setenv("AUTH_CALLOUT_NATS_XKEY_SEED", xKey)
	// Use malformed syntax so startup fails synchronously; valid unavailable URLs now reconnect.
	t.Setenv("AUTH_CALLOUT_NATS_URL", "nats://%zz")
	t.Setenv("AUTH_CALLOUT_PERMISSIONS_FILE", permissionsFile)
	t.Setenv("AUTH_CALLOUT_OBSERVABILITY_METRICS_ENABLED", "false")
	t.Setenv("AUTH_CALLOUT_OBSERVABILITY_TRACING_ENABLED", "false")

	logs := captureStderr(t, func() {
		globalConfig = appconfig.New(defaultConfigYAML)
		err := runServer(&cobra.Command{}, nil)
		require.ErrorContains(t, err, "error connecting to NATS")
	})

	for _, seed := range []string{natsKey, issuerKey, xKey} {
		require.NotContains(t, logs, seed)
	}
}

func TestDefaultConfigIncludesJWKSSettings(t *testing.T) {
	// Load overlays env and config-file sources after defaultConfigYAML; clear
	// those inputs so this test only proves the embedded app defaults.
	t.Setenv("AUTH_CALLOUT_CONFIG", "")
	t.Setenv("AUTH_CALLOUT_CONFIG_FILE", "")
	t.Setenv("AUTH_CALLOUT_HOT_CONFIG", "")
	t.Setenv("AUTH_CALLOUT_JWKS_SIGNING_ALGORITHMS", "")
	t.Setenv("JWKS_SIGNING_ALGORITHMS", "")
	t.Setenv("AUTH_CALLOUT_JWKS_ALLOW_INSECURE", "")
	t.Setenv("JWKS_ALLOW_INSECURE", "")

	manager := appconfig.New(defaultConfigYAML)
	require.NoError(t, manager.Load())

	type jwksConfig struct {
		JWKS struct {
			SigningAlgorithms []string `koanf:"signing-algorithms"`
			AllowInsecure     bool     `koanf:"allow-insecure"`
		} `koanf:"jwks"`
	}
	var config jwksConfig
	require.NoError(t, manager.Unmarshal(&config))
	require.Equal(t, []string{"RS256"}, config.JWKS.SigningAlgorithms)
	require.False(t, config.JWKS.AllowInsecure)
}

func TestSVIDIsOffUnlessConfigured(t *testing.T) {
	tests := []struct {
		name   string
		config string
		want   *auth.SVIDConfig
	}{
		{name: "no svid block", config: "host:\n  port: 8000\n"},
		{
			name: "svid block",
			config: `svid:
  jwks-url: "http://identity-keys.example.com:8080/.well-known/jwks.json"
  allow-insecure: true
  issuers: ["https://issuer.example.com"]
  audience: "dsx-exchange"
  signing-algorithms: ["ES256"]
`,
			want: &auth.SVIDConfig{
				JWKSURL:           "http://identity-keys.example.com:8080/.well-known/jwks.json",
				AllowInsecure:     true,
				Issuers:           []string{"https://issuer.example.com"},
				Audience:          "dsx-exchange",
				SigningAlgorithms: []string{"ES256"},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			configFile := filepath.Join(t.TempDir(), "config.yaml")
			require.NoError(t, os.WriteFile(configFile, []byte(test.config), 0o600))
			t.Setenv("AUTH_CALLOUT_CONFIG", configFile)
			t.Setenv("AUTH_CALLOUT_HOT_CONFIG", "")

			manager := appconfig.New(defaultConfigYAML)
			require.NoError(t, manager.Load())
			var config service.ServiceConfig
			require.NoError(t, manager.Unmarshal(&config))
			require.Equal(t, test.want, config.SVID)
		})
	}
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()

	originalStderr := os.Stderr
	reader, writer, err := os.Pipe()
	require.NoError(t, err)

	os.Stderr = writer
	defer func() {
		os.Stderr = originalStderr
	}()

	fn()
	require.NoError(t, writer.Close())

	output, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())

	return string(output)
}

func mustSeed(t *testing.T, create func() (nkeys.KeyPair, error)) string {
	t.Helper()

	keyPair, err := create()
	require.NoError(t, err)

	seed, err := keyPair.Seed()
	require.NoError(t, err)

	return strings.Clone(string(seed))
}
