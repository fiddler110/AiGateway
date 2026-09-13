// Package pseudomap applies a context_pseudonymizer substitution map (real ->
// fake on requests, fake -> real on responses) to text in a single
// leftmost-longest pass, so a value that was just written in is never
// rescanned and substituted again. The request middleware, its response
// reversal, and the handlers' stream reversal all share this matcher.
package pseudomap

import (
	"sort"
	"strings"
)

// Matcher replaces every key of a map with its value. The zero value is not
// usable; a nil *Matcher returns text unchanged.
type Matcher struct {
	keys   []string // longest first, then lexical, so matching is deterministic
	values map[string]string
	first  [256]bool // first bytes of keys, to skip most positions cheaply
	maxLen int
}

// New returns a Matcher for mapping, ignoring empty keys, or nil when there
// is nothing to replace.
func New(mapping map[string]string) *Matcher {
	m := &Matcher{values: make(map[string]string, len(mapping))}
	for k, v := range mapping {
		if k == "" {
			continue
		}
		m.values[k] = v
		m.keys = append(m.keys, k)
		m.first[k[0]] = true
		m.maxLen = max(m.maxLen, len(k))
	}
	if len(m.keys) == 0 {
		return nil
	}
	sort.Slice(m.keys, func(i, j int) bool {
		if len(m.keys[i]) != len(m.keys[j]) {
			return len(m.keys[i]) > len(m.keys[j])
		}
		return m.keys[i] < m.keys[j]
	})
	return m
}

// MaxLen is the length of the longest key.
func (m *Matcher) MaxLen() int {
	if m == nil {
		return 0
	}
	return m.maxLen
}

// ReplaceAll replaces every key in text with its value in one pass: at each
// position the longest key starting there wins, and scanning resumes after
// the replaced key, so replacement values are never rescanned.
func (m *Matcher) ReplaceAll(text string) string {
	out, _ := m.Replace(text, true)
	return out
}

// Replace is ReplaceAll for text that may continue in a later piece. Unless
// final is set, it stops at the first position where the rest of text is a
// proper prefix of some key (only the next piece can decide whether it is
// one, or whether a longer key matches) and returns that suffix as held, for
// the caller to prepend to the next piece. held is shorter than MaxLen and
// starts on a byte where a key could start.
func (m *Matcher) Replace(text string, final bool) (out, held string) {
	if m == nil {
		return text, ""
	}
	var b strings.Builder
	copied := 0
	for i := 0; i < len(text); {
		if !m.first[text[i]] {
			i++
			continue
		}
		rest := text[i:]
		if !final && m.mayExtend(rest) {
			b.WriteString(text[copied:i])
			return b.String(), rest
		}
		key := m.matchAt(rest)
		if key == "" {
			i++
			continue
		}
		b.WriteString(text[copied:i])
		b.WriteString(m.values[key])
		i += len(key)
		copied = i
	}
	if copied == 0 {
		return text, ""
	}
	b.WriteString(text[copied:])
	return b.String(), ""
}

// mayExtend reports whether rest is a proper prefix of some key.
func (m *Matcher) mayExtend(rest string) bool {
	if len(rest) >= m.maxLen {
		return false
	}
	for _, k := range m.keys {
		if len(rest) < len(k) && strings.HasPrefix(k, rest) {
			return true
		}
	}
	return false
}

// matchAt returns the longest key that rest starts with, or "".
func (m *Matcher) matchAt(rest string) string {
	for _, k := range m.keys {
		if strings.HasPrefix(rest, k) {
			return k
		}
	}
	return ""
}
