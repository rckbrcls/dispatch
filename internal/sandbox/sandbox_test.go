package sandbox

import (
	"bytes"
	"slices"
	"strings"
	"testing"
)

func TestSanitizeDropsParentSecret(t *testing.T) {
	// (a) variável secreta do pai (ex. AWS_SECRET_ACCESS_KEY) não aparece na env construída
	parent := []string{
		"PATH=/usr/bin:/bin",
		"HOME=/Users/alice",
		"USER=alice",
		"AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		"OPENAI_API_KEY=sk-proj-1234567890",
		"DATABASE_PASSWORD=supersecret",
		"LC_ALL=en_US.UTF-8",
		"XDG_CONFIG_HOME=/Users/alice/.config",
		"DISPATCH_HOME=/Users/alice/.dispatch",
	}

	sanitized := Sanitize(parent)

	for _, entry := range sanitized {
		k, _, _ := strings.Cut(entry, "=")
		if strings.Contains(k, "SECRET") || strings.Contains(k, "PASSWORD") || strings.Contains(k, "KEY") {
			t.Errorf("secret variable leaked into sanitized env: %s", entry)
		}
	}

	// Verify allowed variables survived
	mustHave := []string{"PATH=/usr/bin:/bin", "HOME=/Users/alice", "USER=alice", "LC_ALL=en_US.UTF-8", "XDG_CONFIG_HOME=/Users/alice/.config", "DISPATCH_HOME=/Users/alice/.dispatch"}
	for _, m := range mustHave {
		if !slices.Contains(sanitized, m) {
			t.Errorf("missing expected variable %q in sanitized env: %v", m, sanitized)
		}
	}
}

func TestSanitizeHonorsDispatchEnvAllow(t *testing.T) {
	// (b) variável em DISPATCH_ENV_ALLOW aparece
	parent := []string{
		"PATH=/usr/bin",
		"HOME=/Users/alice",
		"CUSTOM_TOKEN=secret-token-val",
		"ANOTHER_VAR=my-value",
		"AWS_SECRET_ACCESS_KEY=should-be-dropped",
		"DISPATCH_ENV_ALLOW=CUSTOM_TOKEN, ANOTHER_VAR",
	}

	sanitized := Sanitize(parent)

	if !slices.Contains(sanitized, "CUSTOM_TOKEN=secret-token-val") {
		t.Errorf("expected CUSTOM_TOKEN in sanitized env, got %v", sanitized)
	}
	if !slices.Contains(sanitized, "ANOTHER_VAR=my-value") {
		t.Errorf("expected ANOTHER_VAR in sanitized env, got %v", sanitized)
	}
	for _, entry := range sanitized {
		if strings.HasPrefix(entry, "AWS_SECRET_ACCESS_KEY=") {
			t.Errorf("unallowed secret present in sanitized env: %s", entry)
		}
	}
}

func TestRedactCoversAllPatternsAndPreservesCommonText(t *testing.T) {
	// (d) Redact cobre os padrões acima e não altera texto comum
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "token assignment",
			in:   "export GITHUB_TOKEN=ghp_1234567890abcdef",
			want: "export GITHUB_TOKEN=[REDACTED]",
		},
		{
			name: "token assignment quoted",
			in:   `export GH_TOKEN="ghp_1234567890abcdef"`,
			want: `export GH_TOKEN=[REDACTED]`,
		},
		{
			name: "secret assignment",
			in:   "CLIENT_SECRET=super_secret_val123",
			want: "CLIENT_SECRET=[REDACTED]",
		},
		{
			name: "password assignment",
			in:   "DB_PASSWORD='my_pass_word_456'",
			want: "DB_PASSWORD=[REDACTED]",
		},
		{
			name: "api key assignment",
			in:   "OPENAI_API_KEY=sk-proj-abcdef123456",
			want: "OPENAI_API_KEY=[REDACTED]",
		},
		{
			name: "aws secret access key assignment",
			in:   "AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
			want: "AWS_SECRET_ACCESS_KEY=[REDACTED]",
		},
		{
			name: "authorization header",
			in:   "Authorization: Bearer my-access-token-123",
			want: "Authorization: [REDACTED]",
		},
		{
			name: "authorization header basic",
			in:   "Authorization: Basic dXNlcjpwYXNzd29yZA==",
			want: "Authorization: [REDACTED]",
		},
		{
			name: "bearer standalone",
			in:   "Failed with Bearer tok_9988776655 in response",
			want: "Failed with Bearer [REDACTED] in response",
		},
		{
			name: "rsa private key block",
			in:   "before\n-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA0m...\n-----END RSA PRIVATE KEY-----\nafter",
			want: "before\n[REDACTED]\nafter",
		},
		{
			name: "openssh private key block",
			in:   "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAA...\n-----END OPENSSH PRIVATE KEY-----",
			want: "[REDACTED]",
		},
		{
			name: "generic private key block",
			in:   "-----BEGIN PRIVATE KEY-----\nMIIEvgIBADANBgkqhkiG9w0BAQEFAASCBKgwggSkAgEAAoIBAQC...\n-----END PRIVATE KEY-----",
			want: "[REDACTED]",
		},
		{
			name: "common text unchanged",
			in:   "The quick brown fox jumps over the lazy dog.\nEverything passed successfully (0.42s).",
			want: "The quick brown fox jumps over the lazy dog.\nEverything passed successfully (0.42s).",
		},
		{
			name: "common text mentioning words without secret syntax",
			in:   "User requested a new token from the administrator. Secret sharing is not permitted.",
			want: "User requested a new token from the administrator. Secret sharing is not permitted.",
		},
		{
			name: "certificate not redacted",
			in:   "-----BEGIN CERTIFICATE-----\nMIICv...\n-----END CERTIFICATE-----",
			want: "-----BEGIN CERTIFICATE-----\nMIICv...\n-----END CERTIFICATE-----",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Redact(tc.in)
			if got != tc.want {
				t.Errorf("\ngot:  %q\nwant: %q", got, tc.want)
			}
		})
	}
}

func TestRedactingWriter(t *testing.T) {
	var buf bytes.Buffer
	w := NewRedactingWriter(&buf)
	input := []byte("api_key=secret-val-123\nAuthorization: Bearer xyz\n")
	n, err := w.Write(input)
	if err != nil || n != len(input) {
		t.Fatalf("write: %d, %v", n, err)
	}
	expected := "api_key=[REDACTED]\nAuthorization: [REDACTED]\n"
	if buf.String() != expected {
		t.Errorf("got %q, want %q", buf.String(), expected)
	}
}
