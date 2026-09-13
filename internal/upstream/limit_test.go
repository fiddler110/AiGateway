package upstream_test

import (
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/scottymacleod/aigateway/internal/upstream"
)

// P0.9: LimitReader passes bodies up to the limit and fails, rather than
// truncating, on anything longer, without returning bytes past the limit.
func TestLimitReader(t *testing.T) {
	cases := []struct {
		name    string
		body    int
		limit   int64
		oneByte bool
		wantErr bool
	}{
		{"under", 10, 16, false, false},
		{"exact", 16, 16, false, false},
		{"exact one byte reads", 16, 16, true, false},
		{"one over", 17, 16, false, true},
		{"one over one byte reads", 17, 16, true, true},
		{"far over", 4096, 16, false, true},
		{"unlimited", 4096, 0, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var src io.Reader = strings.NewReader(strings.Repeat("a", tc.body))
			if tc.oneByte {
				src = iotest.OneByteReader(src)
			}
			got, err := io.ReadAll(upstream.LimitReader(src, tc.limit))
			if tc.wantErr {
				if !errors.Is(err, upstream.ErrResponseTooLarge) {
					t.Fatalf("err = %v, want ErrResponseTooLarge", err)
				}
				if int64(len(got)) > tc.limit {
					t.Errorf("read %d bytes past a %d-byte limit", len(got), tc.limit)
				}
				return
			}
			if err != nil || len(got) != tc.body {
				t.Errorf("read %d bytes, err %v; want %d bytes", len(got), err, tc.body)
			}
		})
	}
}
