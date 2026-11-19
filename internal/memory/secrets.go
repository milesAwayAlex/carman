package memory

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// Requirement M6: nothing shaped like a credential is stored. Memory is read
// back into future sessions verbatim, so a secret written once would be
// replayed into every one of them.

var secretShapes = []struct {
	name    string
	pattern *regexp.Regexp
}{
	{"a private key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"an AWS access key", regexp.MustCompile(`\b(AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{"a GitHub token", regexp.MustCompile(`\b(gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{22,})`)},
	{"an API key", regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}`)},
	{"a Slack token", regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`)},
	{"a Google API key", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`)},
	{"a JSON web token", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`)},
	{"a bearer token", regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/-]{20,}`)},
}

// A URL that carries a password, checked separately because documentation
// writes the same shape with a stand-in where the password goes:
// rediss://default:<token>@host is how to connect, not a credential.
var urlWithPassword = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^/\s:@]+:([^/\s:@]{3,})@`)

// placeholder reports whether what stands where a password would is a
// stand-in for one: <token>, $PASSWORD, ${PASS}, {{secret}}, [password],
// ***, or one of the words people write there.
func placeholder(value string) bool {
	if strings.ContainsAny(value[:1], "<$[{%") || strings.Trim(value, "*xX.") == "" {
		return true
	}
	switch strings.ToLower(strings.Trim(value, "_-")) {
	case "password", "passwd", "pass", "pw", "pwd", "token", "secret", "key", "redacted", "changeme":
		return true
	}
	return false
}

// An assignment such as password=..., checked separately because ordinary
// prose also says things like "token: see the vault".
var assignment = regexp.MustCompile(`(?i)\b(password|passwd|secret|token|api[_-]?key|access[_-]?key)\b["']?\s*[:=]\s*["']?([^\s"']{12,})`)

// findSecret reports what kind of credential the text appears to contain.
func findSecret(text string) (kind string, found bool) {
	for _, shape := range secretShapes {
		if shape.pattern.MatchString(text) {
			return shape.name, true
		}
	}
	for _, match := range urlWithPassword.FindAllStringSubmatch(text, -1) {
		if !placeholder(match[1]) {
			return "a URL with a password", true
		}
	}
	for _, match := range assignment.FindAllStringSubmatch(text, -1) {
		// A real secret mixes letters and digits; a word or a path does not.
		value := match[2]
		if strings.IndexFunc(value, unicode.IsDigit) >= 0 && strings.IndexFunc(value, unicode.IsLetter) >= 0 {
			return "an assigned " + strings.ToLower(match[1]), true
		}
	}
	return "", false
}

// checkSecrets refuses the write when any field looks like a credential. The
// error names the kind of credential and never repeats the text.
func checkSecrets(fields ...string) error {
	for _, field := range fields {
		if kind, found := findSecret(field); found {
			return fmt.Errorf("%w: the text looks like it contains %s", ErrSecret, kind)
		}
	}
	return nil
}
