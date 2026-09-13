package pseudonymizer

import (
	"errors"
	"testing"
)

// P0.11: assign never issues one fake to two real values, for any finding
// kind (only IPv4 re-salted before), and fails closed if it can't.
func TestSessionAssignResaltsCollisions(t *testing.T) {
	s := newSession()
	sameAtSaltZero := func(salt int) string {
		if salt == 0 {
			return "fake-0"
		}
		return "fake-other"
	}

	first, err := s.assign("real-a", sameAtSaltZero)
	if err != nil || first != "fake-0" {
		t.Fatalf("first assign = %q, %v", first, err)
	}
	second, err := s.assign("real-b", sameAtSaltZero)
	if err != nil || second == first {
		t.Fatalf("second real value got fake %q (err %v), colliding with %q", second, err, first)
	}
	again, err := s.assign("real-a", func(int) string { return "unused" })
	if err != nil || again != first {
		t.Errorf("repeat assign = %q, %v; want existing fake %q", again, err, first)
	}

	if _, err := s.assign("real-c", func(int) string { return "fake-0" }); !errors.Is(err, errFakeSpaceExhausted) {
		t.Errorf("exhausted salts: err = %v, want errFakeSpaceExhausted", err)
	}
	fwd, rev := s.snapshot()
	if len(fwd) != 2 || len(rev) != 2 || rev[first] != "real-a" || rev[second] != "real-b" {
		t.Errorf("maps not a bijection after assigns: forward %v reverse %v", fwd, rev)
	}
}
