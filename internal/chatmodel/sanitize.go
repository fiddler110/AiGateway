package chatmodel

import "strings"

const maxClientIDLen = 64

// SanitizeClientID strips everything except [A-Za-z0-9._-] and truncates to
// 64 chars, preventing log injection and unsafe DB/cache keys derived from
// client-controlled headers (x-client-id is advisory only, never a security
// boundary, but it still must not corrupt logs or storage keys).
func SanitizeClientID(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
		if b.Len() >= maxClientIDLen {
			break
		}
	}
	out := b.String()
	if len(out) > maxClientIDLen {
		out = out[:maxClientIDLen]
	}
	return out
}
