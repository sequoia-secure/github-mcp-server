package githubapp

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizePEMVariants(t *testing.T) {
	key := newTestKey(t)
	canonical := string(pkcs1PEMBytes(key))
	oneLineEscaped := strings.ReplaceAll(canonical, "\n", `\n`)

	variants := map[string]string{
		"canonical":                 canonical,
		"literal backslash-n":       oneLineEscaped,
		"double-escaped":            strings.ReplaceAll(canonical, "\n", `\\n`),
		"double-quoted":             `"` + oneLineEscaped + `"`,
		"single-quoted":             `'` + oneLineEscaped + `'`,
		"crlf":                      strings.ReplaceAll(canonical, "\n", "\r\n"),
		"surrounding whitespace":    "\n\n  " + canonical + "  \n",
		"flattened with spaces":     strings.TrimSpace(strings.ReplaceAll(canonical, "\n", " ")),
		"base64 of the file":        base64.StdEncoding.EncodeToString([]byte(canonical)),
		"base64 wrapped at 76 cols": wrap(base64.StdEncoding.EncodeToString([]byte(canonical)), 76),
		"pkcs8":                     string(pkcs8PEM(t, key)),
	}
	for name, in := range variants {
		t.Run(name, func(t *testing.T) {
			got, err := parsePrivateKey(NormalizePEM(in))
			require.NoError(t, err)
			assert.Equal(t, key.N, got.N, "must recover the same key")
		})
	}
}

func TestNormalizePEMLeavesGarbageForTheParser(t *testing.T) {
	for _, in := range []string{"", "not a key", "/secrets/github-app.pem", base64.StdEncoding.EncodeToString([]byte("still not a key"))} {
		_, err := parsePrivateKey(NormalizePEM(in))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no PEM block found")
		assert.Contains(t, err.Error(), "begin-marker=no", "the error describes the shape")
	}
}

func TestParsePrivateKeyErrorNeverEchoesTheValue(t *testing.T) {
	secretish := "-----BEGIN RSA PRIVATE KEY-----\nSUPERSECRETBODY\n-----END RSA PRIVATE KEY-----\n"
	_, err := parsePrivateKey([]byte(secretish))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "SUPERSECRETBODY")
}

func wrap(s string, width int) string {
	var b strings.Builder
	for i := 0; i < len(s); i += width {
		end := i + width
		if end > len(s) {
			end = len(s)
		}
		b.WriteString(s[i:end])
		b.WriteByte('\n')
	}
	return b.String()
}
