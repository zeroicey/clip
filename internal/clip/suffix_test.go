package clip

import (
	"strings"
	"testing"
)

func TestValidateSuffixAccepts(t *testing.T) {
	for _, s := range []string{
		"a",
		"ops1",
		"x-y_z",
		"0",
		"abc-123_xyz",
		strings.Repeat("a", SuffixMaxLen),
	} {
		if err := ValidateSuffix(s); err != nil {
			t.Errorf("ValidateSuffix(%q) = %v, want nil", s, err)
		}
	}
}

func TestValidateSuffixRejects(t *testing.T) {
	cases := []struct {
		suffix string
		reason string
	}{
		{"", "empty"},
		{"API", "uppercase"},
		{"Ops", "mixed case"},
		{"a.b", "dot (looks like a file)"},
		{"a b", "space"},
		{"-lead", "leading hyphen"},
		{"_lead", "leading underscore"},
		{"a/b", "slash"},
		{"后缀", "non-ascii"},
		{strings.Repeat("a", SuffixMaxLen+1), "too long"},
		{"api", "reserved"},
		{"assets", "reserved"},
		{"raw", "reserved (the raw view is a sub-path)"},
		{"health", "reserved"},
		{"favicon", "reserved"},
		{"admin", "reserved"},
	}
	for _, tc := range cases {
		if err := ValidateSuffix(tc.suffix); err == nil {
			t.Errorf("ValidateSuffix(%q) = nil, want an error (%s)", tc.suffix, tc.reason)
		}
	}
}

// TestReservedSuffixesAreNotAddressable is the guard that matters: a suffix the
// router owns must be refused BEFORE it can be written, or it becomes a clip
// that can be created but never opened.
func TestReservedSuffixesAreNotAddressable(t *testing.T) {
	for name := range reservedSuffixes {
		if err := ValidateSuffix(name); err == nil {
			t.Errorf("reserved suffix %q passed validation", name)
		}
	}
}

func TestSuggestSuffixShape(t *testing.T) {
	s := &Store{}
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		got := s.SuggestSuffix(6)
		if len(got) != 6 {
			t.Fatalf("SuggestSuffix(6) = %q (len %d), want len 6", got, len(got))
		}
		if err := ValidateSuffix(got); err != nil {
			t.Fatalf("suggested suffix %q does not validate: %v", got, err)
		}
		// The alphabet excludes look-alikes (0/O, 1/l/I) on purpose: a suffix is
		// read off one screen and typed into another.
		if strings.ContainsAny(got, "01lo") {
			t.Fatalf("suggested suffix %q contains an ambiguous character", got)
		}
		seen[got] = true
	}
	// Not a randomness test — just proof the generator is not returning one
	// constant, which is the failure mode a `readRandom` error fallback would
	// produce.
	if len(seen) < 100 {
		t.Errorf("200 suggestions produced only %d distinct values", len(seen))
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		0:             "0 B",
		512:           "512 B",
		1024:          "1 KiB",
		256 << 10:     "256 KiB",
		5 << 20:       "5 MiB",
		1 << 30:       "1 GiB",
		3 * (1 << 20): "3 MiB",
	}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}
