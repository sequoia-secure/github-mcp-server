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

	// DefaultLogin, when set, names the installation used for requests that
	// do not identify an account (for example an unqualified search). It must
	// be a key of Installations.
	DefaultLogin string
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
	providers    map[string]*Provider
	defaultLogin string
	logger       *slog.Logger

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
	defaultLogin := strings.ToLower(strings.TrimSpace(cfg.DefaultLogin))
	if defaultLogin != "" {
		if _, ok := providers[defaultLogin]; !ok {
			return nil, fmt.Errorf("default installation %q is not among the configured installations", cfg.DefaultLogin)
		}
	}
	return &MultiProvider{
		providers:    providers,
		defaultLogin: defaultLogin,
		logger:       logger,
		warned:       map[string]bool{},
	}, nil
}

// Has reports whether an installation is configured for login.
func (m *MultiProvider) Has(login string) bool {
	_, ok := m.providers[strings.ToLower(login)]
	return ok
}

// Logins returns the configured account logins, sorted.
func (m *MultiProvider) Logins() []string {
	out := make([]string, 0, len(m.providers))
	for login := range m.providers {
		out = append(out, login)
	}
	sort.Strings(out)
	return out
}

// DefaultLogin returns the login used when a request names no account, or
// "" when there is none.
func (m *MultiProvider) DefaultLogin() string {
	return m.defaultLogin
}

// AccessToken returns the installation token for the account recorded in
// ctx, falling back to the default installation. It returns "" (so the
// request goes out unauthenticated and fails at GitHub rather than with the
// wrong tenant's credential) when the account is unknown and no default is
// configured. Tool calls are expected to be validated up front by the
// routing middleware, so this path is a safety net, logged once per login.
func (m *MultiProvider) AccessToken(ctx context.Context) string {
	login, ok := ghcontext.GetInstallationOwner(ctx)
	if !ok {
		login = m.defaultLogin
	}
	p, found := m.providers[strings.ToLower(login)]
	if !found {
		m.mu.Lock()
		if !m.warned[login] {
			m.warned[login] = true
			m.logger.Error("no GitHub App installation configured for account; request will be unauthenticated",
				"login", login, "configured", m.Logins())
		}
		m.mu.Unlock()
		return ""
	}
	return p.AccessToken()
}
