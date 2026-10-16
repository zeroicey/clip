// Package clip implements Relay's network clipboard: an anonymous,
// suffix-addressed place to put text (and a few images) so that it can be
// picked up on another machine.
//
// The shape of the whole module follows from one use case: a person is on
// machine A with a shell open and needs a command, a log excerpt or a screenshot
// to exist on machine B, which has a browser and nothing else. So:
//
//   - reading is always anonymous (no account on either machine),
//   - the address is whatever the person typed, so URLs stay short enough to
//     retype from one screen to another,
//   - writing needs a token, so a guessed suffix can be read but not defaced,
//   - and content is rendered as text, never as markup, because the author is
//     not authenticated and therefore not trusted at all.
package clip

import (
	"fmt"
	"strings"
)

// SuffixMinLen and SuffixMaxLen bound the addressable name.
//
// The lower bound is 1 rather than something longer on purpose: the value of
// this module is that the URL is short and memorable, and the token requirement
// means a guessable suffix is only guessable for *reading*. The upper bound is
// what keeps a suffix from being used as a (tiny, index-hostile) paste buffer
// in its own right.
const (
	SuffixMinLen = 1
	SuffixMaxLen = 32
)

// reservedSuffixes are names the router owns, or which would be confusing or
// ambiguous as a clipboard address.
//
// This list is a hard guard, not a hint: a suffix that collides with a route
// would be unreachable in one direction or the other depending on routing
// details, and a suffix that looks like a file (contains a dot) invites
// confusion with static assets.
var reservedSuffixes = map[string]struct{}{
	"api":       {}, // the JSON API lives here
	"assets":    {}, // static assets, if this service ever serves any
	"static":    {},
	"health":    {}, // also /api/health; both refused so neither is ambiguous
	"robots":    {},
	"favicon":   {},
	"llms":      {},
	"ai":        {}, // keeps a future agent-facing doc from colliding
	"s":         {},
	"k":         {},
	"raw":       {}, // the raw view is /<suffix>/raw
	"admin":     {},
	"login":     {},
	"logout":    {},
	"index":     {},
	"new":       {},
	"null":      {},
	"undefined": {},
}

// IsReserved reports whether a (already valid) suffix is one of the names this
// service keeps for itself.
func IsReserved(suffix string) bool {
	_, ok := reservedSuffixes[suffix]
	return ok
}

// ValidateSuffix checks a user-typed suffix and returns a human-readable reason
// when it is not usable.
//
// The reason is returned rather than just a bool because the intended reader is
// the person who typed it: "只能用小写字母、数字、- 和 _，最长 32 个字符" is
// actionable, "invalid" is not.
func ValidateSuffix(s string) error {
	if s == "" {
		return fmt.Errorf("后缀不能为空")
	}
	if len(s) > SuffixMaxLen {
		return fmt.Errorf("后缀最长 %d 个字符（当前 %d 个）", SuffixMaxLen, len(s))
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
			// allowed anywhere except the first position for - and _
		default:
			switch {
			case c >= 'A' && c <= 'Z':
				return fmt.Errorf("后缀只能用小写字母（%q 要写成小写）", string(c))
			default:
				return fmt.Errorf("后缀只能包含小写字母、数字、- 和 _")
			}
		}
	}
	// A leading separator reads like a flag or a hidden file in a URL, and it
	// makes the "is this a reserved path?" question harder to answer at a
	// glance. Require an alphanumeric first character.
	if !isLowerAlnum(s[0]) {
		return fmt.Errorf("后缀必须以小写字母或数字开头")
	}
	if IsReserved(s) {
		return fmt.Errorf("%q 是系统保留名，换一个", s)
	}
	return nil
}

func isLowerAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}

// SuggestSuffix returns a short random-looking suffix for the "give me one"
// button, using a alphabet with no visually ambiguous characters, so it can be
// read off one screen and typed into another.
func (s *Store) SuggestSuffix(n int) string {
	const alphabet = "abcdefghjkmnpqrstuvwxyz23456789"
	if n <= 0 {
		n = 6
	}
	var b strings.Builder
	b.Grow(n)
	buf := make([]byte, n)
	if _, err := readRandom(buf); err != nil {
		// Fall back to a time-derived name rather than failing the request: a
		// suggestion is a convenience, and the user can always type their own.
		return "clip"
	}
	for i := 0; i < n; i++ {
		b.WriteByte(alphabet[int(buf[i])%len(alphabet)])
	}
	return b.String()
}
