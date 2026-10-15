package sandbox

import (
	"io"
	"regexp"
)

var (
	// privateKeyRe matches PEM private key blocks.
	privateKeyRe = regexp.MustCompile(`(?s)-----BEGIN[^\r\n]*?PRIVATE KEY-----.*?-----END[^\r\n]*?-----`)

	// authHeaderRe matches Authorization: ... headers.
	authHeaderRe = regexp.MustCompile(`(?i)\b(Authorization:\s*)[^\r\n"']+`)

	// bearerRe matches Bearer <token>.
	bearerRe = regexp.MustCompile(`(?i)\b(Bearer\s+)[^\s"',;]+`)

	// secretAssignRe matches assignments to *_TOKEN, *_SECRET, *_PASSWORD, *_API_KEY.
	secretAssignRe = regexp.MustCompile(`(?i)\b([A-Za-z0-9_]*(?:TOKEN|SECRET|PASSWORD|API_KEY)[A-Za-z0-9_]*\s*=\s*)(?:"[^"\r\n]*"|'[^'\r\n]*'|[^\s"',;]+)`)
)

// Redact masks sensitive values (tokens, secrets, passwords, API keys,
// Authorization headers, Bearer tokens, and private key blocks) while
// preserving ordinary text.
func Redact(s string) string {
	if s == "" {
		return ""
	}
	s = privateKeyRe.ReplaceAllString(s, "[REDACTED]")
	s = authHeaderRe.ReplaceAllString(s, "${1}[REDACTED]")
	s = bearerRe.ReplaceAllString(s, "${1}[REDACTED]")
	s = secretAssignRe.ReplaceAllString(s, "${1}[REDACTED]")
	return s
}

// RedactingWriter wraps an io.Writer and applies Redact to all written bytes.
type RedactingWriter struct {
	w io.Writer
}

// NewRedactingWriter returns an io.Writer wrapping w that redacts secrets.
func NewRedactingWriter(w io.Writer) *RedactingWriter {
	return &RedactingWriter{w: w}
}

func (rw *RedactingWriter) Write(p []byte) (int, error) {
	redacted := Redact(string(p))
	if _, err := rw.w.Write([]byte(redacted)); err != nil {
		return 0, err
	}
	return len(p), nil
}
