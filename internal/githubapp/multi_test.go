package githubapp

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ghcontext "github.com/github/github-mcp-server/pkg/context"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseInstallations(t *testing.T) {
	tests := []struct {
		name    string
		spec    string
		want    map[string]string
		wantErr string
	}{
		{"single", "acme=111", map[string]string{"acme": "111"}, ""},
		{"several with spaces and case", " Acme = 111 , beta=222,", map[string]string{"acme": "111", "beta": "222"}, ""},
		{"empty", "", nil, "no installations configured"},
		{"only commas", ",,", nil, "no installations configured"},
		{"missing id", "acme=", nil, "invalid installation"},
		{"missing login", "=111", nil, "invalid installation"},
		{"no separator", "acme", nil, "invalid installation"},
		{"duplicate login differing by case", "acme=1,ACME=2", nil, "more than once"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseInstallations(tt.spec)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// multiInstallationServer serves a distinct token per installation ID and
// counts token requests per installation.
func multiInstallationServer(t *testing.T, tokens map[string]string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		assert.Equal(t, http.MethodPost, r.Method)
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		// app/installations/{id}/access_tokens
		require.Len(t, parts, 4)
		tok, ok := tokens[parts[2]]
		require.True(t, ok, "unexpected installation id %q", parts[2])
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      tok,
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
}

func TestNewMultiProviderValidation(t *testing.T) {
	key := newTestKey(t)
	base := MultiConfig{AppID: "123", PrivateKeyPEM: pkcs1PEMBytes(key), BaseRESTURL: "https://api.example.test/"}

	t.Run("requires installations", func(t *testing.T) {
		_, err := NewMultiProvider(base, quietLogger())
		require.ErrorContains(t, err, "installations are required")
	})

	t.Run("default must be configured", func(t *testing.T) {
		cfg := base
		cfg.Installations = map[string]string{"acme": "1"}
		cfg.DefaultLogin = "other"
		_, err := NewMultiProvider(cfg, quietLogger())
		require.ErrorContains(t, err, `default installation "other"`)
	})

	t.Run("per-installation config errors name the login", func(t *testing.T) {
		cfg := base
		cfg.AppID = ""
		cfg.Installations = map[string]string{"acme": "1"}
		_, err := NewMultiProvider(cfg, quietLogger())
		require.ErrorContains(t, err, `installation for "acme"`)
	})

	t.Run("valid", func(t *testing.T) {
		cfg := base
		cfg.Installations = map[string]string{"Acme": "1", "beta": "2"}
		cfg.DefaultLogin = "ACME"
		mp, err := NewMultiProvider(cfg, quietLogger())
		require.NoError(t, err)
		assert.Equal(t, []string{"acme", "beta"}, mp.Logins())
		assert.Equal(t, "acme", mp.DefaultLogin())
		assert.True(t, mp.Has("acme"))
		assert.True(t, mp.Has("BETA"))
		assert.False(t, mp.Has("gamma"))
	})
}

func TestMultiProviderSelectsTokenByContext(t *testing.T) {
	key := newTestKey(t)
	srv, calls := multiInstallationServer(t, map[string]string{"111": "ghs_acme", "222": "ghs_beta"})

	mp, err := NewMultiProvider(MultiConfig{
		AppID:         "123",
		PrivateKeyPEM: pkcs1PEMBytes(key),
		BaseRESTURL:   srv.URL + "/",
		Installations: map[string]string{"acme": "111", "beta": "222"},
		DefaultLogin:  "acme",
	}, quietLogger())
	require.NoError(t, err)

	acmeCtx := ghcontext.WithInstallationOwner(context.Background(), "acme")
	betaCtx := ghcontext.WithInstallationOwner(context.Background(), "Beta")

	assert.Equal(t, "ghs_acme", mp.AccessToken(acmeCtx))
	assert.Equal(t, "ghs_beta", mp.AccessToken(betaCtx), "login lookup is case-insensitive")
	assert.Equal(t, "ghs_acme", mp.AccessToken(context.Background()), "no owner falls back to the default")
	assert.Equal(t, int32(2), calls.Load(), "one token request per installation")

	// Cached on repeat.
	assert.Equal(t, "ghs_beta", mp.AccessToken(betaCtx))
	assert.Equal(t, int32(2), calls.Load())

	// Unknown owner: no credential, no token request for a made-up installation.
	unknownCtx := ghcontext.WithInstallationOwner(context.Background(), "stranger")
	assert.Equal(t, "", mp.AccessToken(unknownCtx))
	assert.Equal(t, "", mp.AccessToken(unknownCtx))
	assert.Equal(t, int32(2), calls.Load())
}

func TestMultiProviderNoDefaultReturnsEmptyForUnscopedRequests(t *testing.T) {
	key := newTestKey(t)
	srv, calls := multiInstallationServer(t, map[string]string{"111": "ghs_acme"})

	mp, err := NewMultiProvider(MultiConfig{
		AppID:         "123",
		PrivateKeyPEM: pkcs1PEMBytes(key),
		BaseRESTURL:   srv.URL + "/",
		Installations: map[string]string{"acme": "111"},
	}, quietLogger())
	require.NoError(t, err)

	assert.Equal(t, "", mp.AccessToken(context.Background()))
	assert.Equal(t, int32(0), calls.Load())
}
