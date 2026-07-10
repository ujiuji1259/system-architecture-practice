package shortcode

import (
	"strings"
	"testing"
)

func TestGenerateLength(t *testing.T) {
	for _, n := range []int{1, 7, 32} {
		code, err := Generate(n)
		if err != nil {
			t.Fatalf("Generate(%d) error: %v", n, err)
		}
		if len(code) != n {
			t.Errorf("Generate(%d) length = %d, want %d", n, len(code), n)
		}
	}
}

func TestGenerateCharset(t *testing.T) {
	code, err := Generate(64)
	if err != nil {
		t.Fatalf("Generate error: %v", err)
	}
	for _, r := range code {
		if !strings.ContainsRune(alphabet, r) {
			t.Errorf("code contains out-of-alphabet rune %q", r)
		}
	}
}

func TestGenerateUnlikelyCollision(t *testing.T) {
	const iterations = 1000
	seen := make(map[string]struct{}, iterations)
	for i := 0; i < iterations; i++ {
		code, err := Generate(DefaultLength)
		if err != nil {
			t.Fatalf("Generate error: %v", err)
		}
		if _, dup := seen[code]; dup {
			t.Fatalf("unexpected collision on %q after %d iterations", code, i)
		}
		seen[code] = struct{}{}
	}
}
