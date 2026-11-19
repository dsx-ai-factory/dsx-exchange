// Copyright 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nats-io/jwt/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/opentelemetry-go-extra/otelzap"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestPermissionsManagerReloadsKubernetesConfigMapVolume(t *testing.T) {
	volumeDir := t.TempDir()
	permissionsFile := filepath.Join(volumeDir, "permissions.json")

	writeKubernetesConfigMapVersion(t, volumeDir, "2026_05_27_00_00_00.000000001", "APP1")

	pm, err := NewPermissionsManager(permissionsFile, testLogger())
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, pm.Close())
	})

	requireNoAuthAccount(t, pm, "APP1")

	writeKubernetesConfigMapVersion(t, volumeDir, "2026_05_27_00_00_01.000000002", "APP2")

	require.Eventually(t, func() bool {
		profile, ok := pm.GetNoAuthProfile()
		return ok && profile.Account == "APP2"
	}, 2*permissionsReloadPollingInterval, 100*time.Millisecond)
}

func TestPermissionsManagerReloadsRewrittenFile(t *testing.T) {
	permissionsFile := filepath.Join(t.TempDir(), "permissions.json")
	writeNoAuthPermissions(t, permissionsFile, "APP1")

	pm, err := NewPermissionsManager(permissionsFile, testLogger())
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, pm.Close())
	})

	requireNoAuthAccount(t, pm, "APP1")

	writeNoAuthPermissions(t, permissionsFile, "APP2")

	require.Eventually(t, func() bool {
		profile, ok := pm.GetNoAuthProfile()
		return ok && profile.Account == "APP2"
	}, 2*permissionsReloadPollingInterval, 100*time.Millisecond)
}

func TestPermissionsManagerHasOAuth2Entries(t *testing.T) {
	tests := []struct {
		name   string
		config PermissionsConfig
		want   bool
	}{
		{"no entries", PermissionsConfig{NoAuth: &NoAuthEntry{Account: "APP"}}, false},
		{
			"oauth2 client",
			PermissionsConfig{OAuth2: map[string]*OAuth2Entry{"app": {Azp: "app-client", Account: "APP"}}},
			true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data, err := json.Marshal(test.config)
			require.NoError(t, err)
			permissionsFile := filepath.Join(t.TempDir(), "permissions.json")
			require.NoError(t, os.WriteFile(permissionsFile, data, 0o600))

			pm, err := NewPermissionsManager(permissionsFile, testLogger())
			require.NoError(t, err)
			t.Cleanup(func() {
				require.NoError(t, pm.Close())
			})

			require.Equal(t, test.want, pm.HasOAuth2Entries())
		})
	}
}

func TestPermissionsManagerLoadsSVIDEntries(t *testing.T) {
	t.Setenv("TEST_SVID_REGION", "region-1")

	pm := newSVIDPermissionsManager(t, map[string]*SVIDEntry{
		"role-rule": {
			SVIDKey:     SVIDKey{Tenant: "tenant-1", Region: "${TEST_SVID_REGION}", Service: "dsx", Partition: "example.com", Role: "svid-test"},
			Account:     "CSC",
			Permissions: jwt.Permissions{Pub: jwt.Permission{Allow: []string{"svid.test.>"}}},
		},
		"service-rule": {
			SVIDKey: SVIDKey{Tenant: "tenant-1", Region: "region-1", Service: "dsx", Partition: "example.com"},
			Account: "CSC",
			Permissions: jwt.Permissions{
				Pub: jwt.Permission{Deny: []string{"svid.secret.>"}},
				Sub: jwt.Permission{Allow: []string{"svid.any.>"}},
			},
		},
	})
	require.True(t, pm.HasSVIDEntries())

	dsx := SVIDKey{Tenant: "tenant-1", Region: "region-1", Service: "dsx", Partition: "example.com"}
	withRole := func(role string) SVIDKey {
		key := dsx
		key.Role = role
		return key
	}

	tests := []struct {
		name     string
		key      SVIDKey
		wantRule string
		wantPub  jwt.Permission
		wantSub  jwt.Permission
	}{
		{
			name:     "role rule",
			key:      withRole("svid-test"),
			wantRule: "role-rule",
			wantPub:  jwt.Permission{Allow: []string{"svid.test.>"}},
		},
		{
			name:     "rule without a role",
			key:      dsx,
			wantRule: "service-rule",
			wantPub:  jwt.Permission{Deny: []string{"svid.secret.>"}},
			wantSub:  jwt.Permission{Allow: []string{"svid.any.>"}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			profile, ok := pm.GetSVIDProfile(test.key)
			require.True(t, ok)
			assert.Equal(t, test.wantRule, profile.Name)
			assert.Equal(t, "CSC", profile.Account)
			assert.Equal(t, test.wantPub, profile.Permissions.Pub)
			assert.Equal(t, test.wantSub, profile.Permissions.Sub)
		})
	}

	t.Run("other role has no rule", func(t *testing.T) {
		_, ok := pm.GetSVIDProfile(withRole("other"))
		assert.False(t, ok)
	})

	t.Run("other service has no rule", func(t *testing.T) {
		key := withRole("svid-test")
		key.Service = "other"
		_, ok := pm.GetSVIDProfile(key)
		assert.False(t, ok)
	})
}

// The identity fields of an svid entry sit next to account in the JSON, and
// each of them expands ${VAR}.
func TestPermissionsManagerReadsFlatSVIDEntryJSON(t *testing.T) {
	t.Setenv("TEST_SVID_TENANT", "tenant-1")
	t.Setenv("TEST_SVID_REGION", "region-1")
	t.Setenv("TEST_SVID_SERVICE", "dsx")
	t.Setenv("TEST_SVID_PARTITION", "example.com")
	t.Setenv("TEST_SVID_ROLE", "svid-test")

	path := filepath.Join(t.TempDir(), "permissions.json")
	require.NoError(t, os.WriteFile(path, []byte(`{
  "svid": {
    "rule": {
      "tenant": "${TEST_SVID_TENANT}",
      "region": "${TEST_SVID_REGION}",
      "service": "${TEST_SVID_SERVICE}",
      "partition": "${TEST_SVID_PARTITION}",
      "role": "${TEST_SVID_ROLE}",
      "account": "CSC",
      "permissions": {"pub": {"allow": ["svid.test.>"]}}
    }
  }
}`), 0o600))
	pm, err := NewPermissionsManager(path, testLogger())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pm.Close()) })

	key := SVIDKey{Tenant: "tenant-1", Region: "region-1", Service: "dsx", Partition: "example.com", Role: "svid-test"}
	profile, ok := pm.GetSVIDProfile(key)
	require.True(t, ok)
	assert.Equal(t, "rule", profile.Name)
	assert.Equal(t, jwt.Permission{Allow: []string{"svid.test.>"}}, profile.Permissions.Pub)

	key.Role = "other"
	_, ok = pm.GetSVIDProfile(key)
	assert.False(t, ok, "the rule must keep its role")
}

func TestPermissionsManagerGetSVIDProfileBeforeLoad(t *testing.T) {
	pm := &PermissionsManager{}

	for _, role := range []string{"svid-test", ""} {
		key := SVIDKey{Tenant: "tenant-1", Region: "region-1", Service: "dsx", Partition: "example.com", Role: role}
		profile, ok := pm.GetSVIDProfile(key)
		assert.False(t, ok, "role %q", role)
		assert.Equal(t, UserProfile{}, profile, "role %q", role)
	}
}

func TestPermissionsManagerRejectsInvalidSVIDEntries(t *testing.T) {
	entry := func(role string) *SVIDEntry {
		return &SVIDEntry{
			SVIDKey: SVIDKey{Tenant: "tenant-1", Region: "region-1", Service: "dsx", Partition: "example.com", Role: role},
			Account: "CSC",
		}
	}

	tests := []struct {
		name      string
		entries   map[string]*SVIDEntry
		wantError string
	}{
		{
			name:      "duplicate identity",
			entries:   map[string]*SVIDEntry{"first": entry("svid-test"), "second": entry("svid-test")},
			wantError: "SVID entries 'first' and 'second' match the same identity",
		},
		{
			name:      "duplicate service-wide identity",
			entries:   map[string]*SVIDEntry{"first": entry(""), "second": entry("")},
			wantError: "SVID entries 'first' and 'second' match the same identity",
		},
		{
			name:      "missing partition",
			entries:   map[string]*SVIDEntry{"no-partition": {SVIDKey: SVIDKey{Tenant: "tenant-1", Region: "region-1", Service: "dsx"}, Account: "CSC"}},
			wantError: "SVID entry 'no-partition' must set tenant, region, service, partition and account",
		},
		{
			name:      "missing account",
			entries:   map[string]*SVIDEntry{"no-account": {SVIDKey: SVIDKey{Tenant: "tenant-1", Region: "region-1", Service: "dsx", Partition: "example.com"}}},
			wantError: "SVID entry 'no-account' must set tenant, region, service, partition and account",
		},
		{
			name:      "unset variable",
			entries:   map[string]*SVIDEntry{"unset": {SVIDKey: SVIDKey{Tenant: "${TEST_SVID_UNSET}", Region: "region-1", Service: "dsx", Partition: "example.com"}, Account: "CSC"}},
			wantError: "SVID entry 'unset' must set tenant",
		},
		{
			name:      "null entry",
			entries:   map[string]*SVIDEntry{"null": nil},
			wantError: "SVID entry 'null' is empty",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewPermissionsManager(writeSVIDPermissions(t, test.entries), testLogger())
			require.ErrorContains(t, err, test.wantError)
		})
	}
}

func TestPermissionsManagerKeepsSVIDRulesWhenReloadFails(t *testing.T) {
	rule := &SVIDEntry{SVIDKey: SVIDKey{Tenant: "tenant-1", Region: "region-1", Service: "dsx", Partition: "example.com"}, Account: "CSC"}
	path := writeSVIDPermissions(t, map[string]*SVIDEntry{"rule": rule})
	core, logs := observer.New(zap.ErrorLevel)
	pm, err := NewPermissionsManager(path, otelzap.New(zap.New(core)))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pm.Close()) })

	data, err := json.Marshal(PermissionsConfig{SVID: map[string]*SVIDEntry{"a": rule, "b": rule}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	require.Eventually(t, func() bool {
		return logs.FilterMessage("Error reloading config").Len() > 0
	}, 2*permissionsReloadPollingInterval, 100*time.Millisecond)
	assert.Contains(t, logs.All()[0].ContextMap()["error"], "match the same identity")

	profile, ok := pm.GetSVIDProfile(SVIDKey{Tenant: "tenant-1", Region: "region-1", Service: "dsx", Partition: "example.com"})
	require.True(t, ok)
	assert.Equal(t, "rule", profile.Name)
}

func newSVIDPermissionsManager(t *testing.T, entries map[string]*SVIDEntry) *PermissionsManager {
	t.Helper()

	pm, err := NewPermissionsManager(writeSVIDPermissions(t, entries), testLogger())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pm.Close()) })
	return pm
}

func writeSVIDPermissions(t *testing.T, entries map[string]*SVIDEntry) string {
	t.Helper()

	data, err := json.Marshal(PermissionsConfig{SVID: entries})
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "permissions.json")
	require.NoError(t, os.WriteFile(path, data, 0o600))
	return path
}

func testLogger() *otelzap.Logger {
	return otelzap.New(zap.NewNop())
}

func writeKubernetesConfigMapVersion(t *testing.T, volumeDir, version, account string) {
	t.Helper()

	versionName := ".." + version
	versionDir := filepath.Join(volumeDir, versionName)
	require.NoError(t, os.Mkdir(versionDir, 0o755))
	writeNoAuthPermissions(t, filepath.Join(versionDir, "permissions.json"), account)

	tmpLink := filepath.Join(volumeDir, "..data_tmp")
	require.NoError(t, os.RemoveAll(tmpLink))
	require.NoError(t, os.Symlink(versionName, tmpLink))
	require.NoError(t, os.Rename(tmpLink, filepath.Join(volumeDir, "..data")))

	permissionsLink := filepath.Join(volumeDir, "permissions.json")
	if _, err := os.Lstat(permissionsLink); os.IsNotExist(err) {
		require.NoError(t, os.Symlink(filepath.Join("..data", "permissions.json"), permissionsLink))
	} else {
		require.NoError(t, err)
	}
}

func writeNoAuthPermissions(t *testing.T, path, account string) {
	t.Helper()

	data, err := json.Marshal(PermissionsConfig{
		NoAuth: &NoAuthEntry{
			Account: account,
		},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o644))
}

func requireNoAuthAccount(t *testing.T, pm *PermissionsManager, account string) {
	t.Helper()

	profile, ok := pm.GetNoAuthProfile()
	require.True(t, ok)
	require.Equal(t, account, profile.Account)
}
