package githubapp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	ghcontext "github.com/github/github-mcp-server/pkg/context"
)

// MultiConfig describes one GitHub App installed on several accounts. Each
// installation gets its own cached, self-refreshing token; the token used for
// a request is chosen by the account login recorded in the request context
// (see ghcontext.WithInstallationOwner).
type MultiConfig struct {
	// AppID is used as the JWT issuer. GitHub accepts an app ID or client ID.
	AppID string

	// PrivateKeyPEM is the RSA key used to sign app JWTs.
	PrivateKeyPEM []byte

	// BaseRESTURL is the REST API base, e.g. https://api.github.com/.
	BaseRESTURL string

	// Installations maps an account login (organization or user) to the ID of
	// the app installation on that account. Logins are matched
	// case-insensitively.
	Installations map[string]string
}

// ParseInstallations parses a comma-separated "login=installationID" list,
// the format of the --app-installations flag / GITHUB_APP_INSTALLATIONS.
func ParseInstallations(spec string) (map[string]string, error) {
	out := map[string]string{}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		login, id, ok := strings.Cut(part, "=")
		login, id = strings.TrimSpace(login), strings.TrimSpace(id)
		if !ok || login == "" || id == "" {
			return nil, fmt.Errorf("invalid installation %q: want login=installationID", part)
		}
		key := strings.ToLower(login)
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("installation for %q listed more than once", login)
		}
		out[key] = id
	}
	if len(out) == 0 {
		return nil, errors.New("no installations configured: want login=installationID[,login=installationID...]")
	}
	return out, nil
}

// MultiProvider selects a per-installation token for each request.
type MultiProvider struct {
	providers map[string]*Provider
	logins    []string // sorted keys of providers
	logger    *slog.Logger

	mu     sync.Mutex
	warned map[string]bool
}

// NewMultiProvider builds one Provider per configured installation.
func NewMultiProvider(cfg MultiConfig, logger *slog.Logger) (*MultiProvider, error) {
	if len(cfg.Installations) == 0 {
		return nil, errors.New("GitHub App installations are required (GITHUB_APP_INSTALLATIONS)")
	}
	if logger == nil {
		logger = slog.Default()
	}
	providers := make(map[string]*Provider, len(cfg.Installations))
	for login, id := range cfg.Installations {
		p, err := NewProvider(Config{
			AppID:          cfg.AppID,
			InstallationID: id,
			PrivateKeyPEM:  cfg.PrivateKeyPEM,
			BaseRESTURL:    cfg.BaseRESTURL,
		}, logger.With("installation_owner", login))
		if err != nil {
			return nil, fmt.Errorf("installation for %q: %w", login, err)
		}
		providers[strings.ToLower(login)] = p
	}
	logins := make([]string, 0, len(providers))
	for login := range providers {
		logins = append(logins, login)
	}
	sort.Strings(logins)
	return &MultiProvider{
		providers: providers,
		logins:    logins,
		logger:    logger,
		warned:    map[string]bool{},
	}, nil
}

// Has reports whether an installation is configured for login.
func (m *MultiProvider) Has(login string) bool {
	_, ok := m.providers[strings.ToLower(login)]
	return ok
}

// Logins returns the configured account logins, sorted.
func (m *MultiProvider) Logins() []string {
	return append([]string(nil), m.logins...)
}

// AccessToken returns the installation token for the account recorded in
// ctx. Requests that carry no account (the server's own housekeeping calls;
// tool calls always carry one after the routing middleware) use the first
// configured installation, so they are still authenticated. It returns ""
// for an unknown account, so such a request goes out unauthenticated and
// fails at GitHub rather than with another tenant's credential; the routing
// middleware rejects those calls up front, so this is a safety net, logged
// once per login.
func (m *MultiProvider) AccessToken(ctx context.Context) string {
	login, ok := ghcontext.GetInstallationOwner(ctx)
	if !ok {
		login = m.logins[0]
	}
	p, found := m.providers[strings.ToLower(login)]
	if !found {
		m.mu.Lock()
		if !m.warned[login] {
			m.warned[login] = true
			m.logger.Error("no GitHub App installation configured for account; request will be unauthenticated",
				"login", login, "configured", m.logins)
		}
		m.mu.Unlock()
		return ""
	}
	return p.AccessToken()
}
