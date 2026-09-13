package pseudonymizer

import (
	"crypto/sha256"
	"fmt"
	"strconv"
	"strings"
)

// hashBytes returns SHA-256(real + ":" + salt) for deterministic,
// session-repeatable fake-value derivation: the same real value always
// produces the same fake within a session (same salt sequence), which is
// what makes reversal possible.
func hashBytes(real string, salt int) []byte {
	h := sha256.Sum256([]byte(real + ":" + strconv.Itoa(salt)))
	return h[:]
}

// docPrefixes are the two RFC 5737 documentation address blocks — safe,
// non-routable ranges to use as fake IPv4 addresses so a leaked fake never
// points at a real host.
var docPrefixes = []string{"203.0.113.", "198.51.100."}

// fakeIPv4 derives a deterministic fake from real, re-salting up to 511
// times if the candidate collides with a DIFFERENT real value already
// assigned that fake in this session (existingFakes maps fake->real).
// Never silently overwrites an existing assignment.
func fakeIPv4(real string, existingFakes map[string]string) string {
	for salt := 0; salt < 512; salt++ {
		h := hashBytes(real, salt)
		prefix := docPrefixes[int(h[0])%2]
		octet := int(h[1])%254 + 1 // 1-254
		candidate := fmt.Sprintf("%s%d", prefix, octet)
		if existing, ok := existingFakes[candidate]; ok && existing != real {
			continue
		}
		return candidate
	}
	// Exhausted the salt space (extremely unlikely): fall back to the
	// last computed candidate rather than looping forever.
	h := hashBytes(real, 511)
	return fmt.Sprintf("%s%d", docPrefixes[int(h[0])%2], int(h[1])%254+1)
}

func fakeIPv6(real string) string {
	h := hashBytes(real, 0)
	return fmt.Sprintf("fd00:db8:%x::%x", h[0:2], h[2:4])
}

// fakeCIDR fakes the network part of a CIDR block, keeping the original
// mask unchanged.
func fakeCIDR(real, mask string, existingFakes map[string]string) string {
	network := strings.TrimSuffix(real, mask)
	return fakeIPv4(network, existingFakes) + mask
}

func fakeHostname(real string) string {
	h := hashBytes(real, 0)
	suffix := ""
	if idx := strings.Index(real, "."); idx != -1 {
		suffix = real[idx:]
	}
	return fmt.Sprintf("svc-%x%s", h[0:3], suffix)
}

// fakePassword substitutes each character while preserving its case class
// (uppercase/lowercase/digit/other) so the fake "looks like" a real
// password of the same shape without revealing the actual value.
func fakePassword(real string) string {
	h := hashBytes(real, 0)
	out := make([]byte, len(real))
	for i := 0; i < len(real); i++ {
		c := real[i]
		hb := h[i%len(h)]
		switch {
		case c >= 'A' && c <= 'Z':
			out[i] = 'A' + hb%26
		case c >= 'a' && c <= 'z':
			out[i] = 'a' + hb%26
		case c >= '0' && c <= '9':
			out[i] = '0' + hb%10
		default:
			out[i] = c
		}
	}
	return string(out)
}

func fakeUsername(real string) string {
	h := hashBytes(real, 0)
	return fmt.Sprintf("user-%x", h[0:2])
}

func fakeSensitiveString(real string) string {
	h := hashBytes(real, 0)
	return fmt.Sprintf("item-%x", h[0:3])
}
