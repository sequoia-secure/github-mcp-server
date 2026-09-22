package githubapp

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
)

// pemBlock matches a PEM header, body and footer regardless of how the body
// is wrapped or separated.
var pemBlock = regexp.MustCompile(`(?s)(-----BEGIN [A-Z0-9 ]+-----)(.*?)(-----END [A-Z0-9 ]+-----)`)

// NormalizePEM turns the common ways an operator pastes a private key into
// an environment variable or a secret store back into canonical PEM:
//
//   - surrounding single or double quotes
//   - "\n" escapes, including double-escaped "\\n"
//   - CRLF or CR line endings
//   - the whole PEM base64-encoded (e.g. `base64 key.pem`)
//   - header, body and footer flattened onto one line with spaces
//
// The body is re-wrapped at 64 columns. Input that contains no PEM header is
// returned trimmed and unescaped so the caller's parser reports it.
func NormalizePEM(raw string) []byte {
	s := strings.TrimSpace(raw)
	if len(s) >= 2 && ((s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'')) {
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	s = strings.ReplaceAll(s, `\\n`, "\n")
	s = strings.ReplaceAll(s, `\n`, "\n")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")

	if !strings.Contains(s, "-----BEGIN") {
		compact := strings.Join(strings.Fields(s), "")
		for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
			if dec, err := enc.DecodeString(compact); err == nil && bytes.Contains(dec, []byte("-----BEGIN")) {
				return NormalizePEM(string(dec))
			}
		}
		return []byte(s)
	}

	m := pemBlock.FindStringSubmatch(s)
	if m == nil {
		return []byte(s)
	}
	body := strings.Join(strings.Fields(m[2]), "")
	var b strings.Builder
	b.WriteString(m[1])
	b.WriteByte('\n')
	for i := 0; i < len(body); i += 64 {
		end := i + 64
		if end > len(body) {
			end = len(body)
		}
		b.WriteString(body[i:end])
		b.WriteByte('\n')
	}
	b.WriteString(m[3])
	b.WriteByte('\n')
	return []byte(b.String())
}

// describePEMShape summarizes a value that failed to parse as PEM without
// revealing its contents, so an operator can tell a wrong secret from a
// mis-encoded one from the server log alone.
func describePEMShape(raw []byte) string {
	s := string(raw)
	has := func(sub string) string {
		if strings.Contains(s, sub) {
			return "yes"
		}
		return "no"
	}
	return fmt.Sprintf("length=%d lines=%d begin-marker=%s end-marker=%s literal-backslash-n=%s",
		len(s), strings.Count(s, "\n")+1, has("-----BEGIN"), has("-----END"), has(`\n`))
}
