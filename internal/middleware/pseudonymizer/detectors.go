package pseudonymizer

import (
	"regexp"
	"strings"
)

// kind identifies which detector found a sensitive value, driving which
// fake-generation function produces its replacement.
type kind int

const (
	kindIPv4 kind = iota
	kindIPv6
	kindCIDR
	kindPassword
	kindConnString
	kindUnixPath
	kindWindowsPath
	kindHostname
	kindSensitiveString
	kindUsername
)

type finding struct {
	kind  kind
	value string // the exact real substring to replace
	extra string // kind-specific auxiliary data (e.g. CIDR mask, path prefix)
}

// Regexes below are all RE2-compatible (no backreferences/lookahead
// needed): reviewed against every pattern class in the reference spec.

var (
	privateIPv4 = regexp.MustCompile(`\b(?:10(?:\.\d{1,3}){3}|172\.(?:1[6-9]|2\d|3[01])(?:\.\d{1,3}){2}|192\.168(?:\.\d{1,3}){2})\b`)
	cidrSuffix  = regexp.MustCompile(`\b(?:10(?:\.\d{1,3}){3}|172\.(?:1[6-9]|2\d|3[01])(?:\.\d{1,3}){2}|192\.168(?:\.\d{1,3}){2})/\d{1,2}\b`)

	privateIPv6ULA       = regexp.MustCompile(`(?i)\bfd[0-9a-f]{2}(?::[0-9a-f]{0,4}){1,7}\b`)
	privateIPv6LinkLocal = regexp.MustCompile(`(?i)\bfe80(?::[0-9a-f]{0,4}){1,7}\b`)

	// Password/secret/token assignment: key = value, value 4+ chars, not
	// starting with an obvious placeholder prefix (checked post-match,
	// RE2 has no negative lookahead).
	secretAssignment = regexp.MustCompile(`(?i)(password|passwd|pwd|secret|token)\s*[:=]\s*['"]?([^\s'",}]{4,})['"]?`)

	connString = regexp.MustCompile(`(?i)(postgres|postgresql|mysql|mongodb|redis|amqp|mariadb)://([^:/\s]+):([^@/\s]+)@`)

	unixHomePath    = regexp.MustCompile(`/home/([A-Za-z0-9_.-]+)/`)
	windowsUserPath = regexp.MustCompile(`(?i)[A-Z]:\\Users\\([A-Za-z0-9_.-]+)\\`)

	hostnamePattern = regexp.MustCompile(`\b[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?)+\b`)
)

var placeholderPrefixes = []string{"your_", "example", "placeholder", "changeme", "<"}

var defaultInternalDomains = []string{".local", ".internal", ".home.lab", ".lan"}
var defaultIgnoreDomains = []string{
	"example.com", "example.org", "example.net", "example.internal",
	"localhost", "github.com", "openai.com", "api.openai.com",
	"api.githubcopilot.com", "anthropic.com",
}

func hasAnySuffix(s string, suffixes []string) bool {
	lower := strings.ToLower(s)
	for _, suf := range suffixes {
		if strings.HasSuffix(lower, strings.ToLower(suf)) {
			return true
		}
	}
	return false
}

func equalsAnyDomain(s string, domains []string) bool {
	lower := strings.ToLower(s)
	for _, d := range domains {
		if lower == strings.ToLower(d) {
			return true
		}
	}
	return false
}

// hasPlaceholderPrefix filters out obviously-fake values like
// "password=your_password_here" that shouldn't be pseudonymized.
func hasPlaceholderPrefix(value string) bool {
	lower := strings.ToLower(value)
	for _, prefix := range placeholderPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

// detect finds every sensitive value in text, given the configured
// internal/ignore domain lists and literal sensitive-strings/usernames
// lists. Order matters for downstream substitution (longest-value-first is
// applied by the caller, not here).
func detect(text string, internalDomains, ignoreDomains, sensitiveStrings, usernames []string) []finding {
	var findings []finding

	// Performance short-circuit: skip IP/hostname/path regex passes
	// entirely if the text lacks characters those patterns require.
	hasDotOrColon := strings.ContainsAny(text, ".:/\\=@")
	hasDigits := strings.ContainsAny(text, "0123456789")

	if hasDotOrColon && hasDigits {
		// CIDR checked BEFORE plain IPv4 so the mask isn't left dangling.
		for _, m := range cidrSuffix.FindAllString(text, -1) {
			slash := strings.LastIndex(m, "/")
			findings = append(findings, finding{kind: kindCIDR, value: m, extra: m[slash:]})
		}
		remaining := cidrSuffix.ReplaceAllString(text, "")
		for _, m := range privateIPv4.FindAllString(remaining, -1) {
			findings = append(findings, finding{kind: kindIPv4, value: m})
		}
		for _, m := range privateIPv6ULA.FindAllString(text, -1) {
			findings = append(findings, finding{kind: kindIPv6, value: m})
		}
		for _, m := range privateIPv6LinkLocal.FindAllString(text, -1) {
			findings = append(findings, finding{kind: kindIPv6, value: m})
		}
	}

	if hasDotOrColon {
		for _, m := range secretAssignment.FindAllStringSubmatch(text, -1) {
			full, value := m[0], m[2]
			if len(value) < 4 || hasPlaceholderPrefix(value) {
				continue
			}
			findings = append(findings, finding{kind: kindPassword, value: value, extra: full})
		}
		for _, m := range connString.FindAllStringSubmatch(text, -1) {
			user, pass := m[2], m[3]
			findings = append(findings, finding{kind: kindConnString, value: user, extra: "user"})
			findings = append(findings, finding{kind: kindConnString, value: pass, extra: "pass"})
		}
		for _, m := range unixHomePath.FindAllStringSubmatch(text, -1) {
			findings = append(findings, finding{kind: kindUnixPath, value: m[1]})
		}
		for _, m := range windowsUserPath.FindAllStringSubmatch(text, -1) {
			findings = append(findings, finding{kind: kindWindowsPath, value: m[1]})
		}

		internal := internalDomains
		if len(internal) == 0 {
			internal = defaultInternalDomains
		}
		ignore := ignoreDomains
		if len(ignore) == 0 {
			ignore = defaultIgnoreDomains
		}
		for _, m := range hostnamePattern.FindAllString(text, -1) {
			if !hasAnySuffix(m, internal) {
				continue
			}
			if equalsAnyDomain(m, ignore) || hasAnySuffix(m, ignore) {
				continue
			}
			findings = append(findings, finding{kind: kindHostname, value: m})
		}
	}

	for _, s := range sensitiveStrings {
		if s != "" && strings.Contains(text, s) {
			findings = append(findings, finding{kind: kindSensitiveString, value: s})
		}
	}
	for _, u := range usernames {
		if u != "" && strings.Contains(text, u) {
			findings = append(findings, finding{kind: kindUsername, value: u})
		}
	}

	return findings
}
