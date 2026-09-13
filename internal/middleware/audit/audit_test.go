package audit

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/scottymacleod/aigateway/internal/pipeline"
)

// P0.14: the audit file used to be reopened for every request and created
// world-readable (0o644) although it holds source IPs. It is now opened once,
// kept open, and created 0o640.
func TestAuditFileOpenedOnceWithRestrictedMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "audit.jsonl")
	mw, err := New(map[string]any{"log_file": path})
	if err != nil {
		t.Fatal(err)
	}
	m := mw.(*Middleware)
	var opens int
	var perms []os.FileMode
	m.openFile = func(name string, flag int, perm os.FileMode) (*os.File, error) {
		opens++
		perms = append(perms, perm)
		return os.OpenFile(name, flag, perm)
	}
	defer m.Close()

	for i := 0; i < 3; i++ {
		gctx := pipeline.NewGatewayContext("client-a", "up", "192.0.2.1")
		if err := m.Finish(context.Background(), gctx); err != nil {
			t.Fatalf("Finish: %v", err)
		}
	}
	if opens != 1 {
		t.Errorf("file opened %d times for 3 records, want 1", opens)
	}
	for _, p := range perms {
		if p != 0o640 {
			t.Errorf("file opened with mode %o, want 640", p)
		}
	}
	// Windows has no Unix permission bits (os.Stat reports 0o666 for any
	// writable file), so the created file's mode is only checked elsewhere;
	// the requested mode above is checked everywhere.
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm&0o007 != 0 {
			t.Errorf("audit file mode %o is readable by others", perm)
		}
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	lines := 0
	for sc := bufio.NewScanner(f); sc.Scan(); lines++ {
		var r Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil || r.SourceIP != "192.0.2.1" {
			t.Errorf("line %q: record %+v, err %v", sc.Text(), r, err)
		}
	}
	if lines != 3 {
		t.Errorf("got %d lines, want 3", lines)
	}
}
