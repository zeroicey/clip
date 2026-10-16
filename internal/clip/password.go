package clip

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Access passwords are stored as an argon2id PHC string — the same
// self-describing format the reference implementation emits:
//
//	$argon2id$v=19$m=19456,t=2,p=1$<b64 salt>$<b64 hash>
//
// Why argon2id and not sha256 (which is what the removed owner token used):
// a token is 128 bits of machine-generated entropy, so hashing it once is
// enough — there is nothing to guess. A password is chosen by a human and is
// therefore guessable, and the whole point of the "not fully public" feature is
// to stop exactly that guessing. A fast hash would make an offline attack
// (anyone who reads the backup, or the operator) trivial, so the hash must be
// deliberately expensive. This is a different threat model, not a stronger
// version of the same one.
//
// The parameters are stored IN the string rather than repeated in code, so
// raising the cost later does not invalidate existing passwords: verification
// reads the parameters the password was hashed with. That is the property that
// makes this format worth parsing instead of storing a bare digest.
const (
	argonMemoryKiB = 19 * 1024 // 19 MiB — OWASP's second recommended setting
	argonTime      = 2
	argonParallel  = 1
	argonSaltBytes = 16
	argonKeyBytes  = 32
)

// Password length bounds. The minimum is a floor against the trivially
// guessable ("a", "123"), not a claim of strength: a 4-character password is
// still weak, and the UI says so rather than pretending otherwise. The maximum
// is a sanity bound — argon2's cost is fixed by its parameters, not by the input
// length, so this is not a DoS defence, just a limit on absurdity.
const (
	PasswordMinLen = 4
	PasswordMaxLen = 128
)

// ErrPasswordTooShort and ErrPasswordTooLong are returned by CheckPassword so
// the API layer can say which bound was violated in the user's own words.
var (
	ErrPasswordTooShort = fmt.Errorf("密码至少 %d 个字符", PasswordMinLen)
	ErrPasswordTooLong  = fmt.Errorf("密码最多 %d 个字符", PasswordMaxLen)
)

// CheckPassword validates a *new* password before it is hashed.
func CheckPassword(plain string) error {
	// Count runes, not bytes: a Chinese passphrase should not be measured as
	// three times longer than it is.
	n := len([]rune(plain))
	if n < PasswordMinLen {
		return ErrPasswordTooShort
	}
	if n > PasswordMaxLen {
		return ErrPasswordTooLong
	}
	return nil
}

// readRandom fills b with cryptographically secure random bytes.
//
// A named helper rather than a bare rand.Read at each site, so the salt
// generation here and the suffix suggestion in suffix.go cannot drift onto
// different sources — one of them being a non-cryptographic source would be a
// silent downgrade.
func readRandom(b []byte) (int, error) { return rand.Read(b) }

// hashPassword returns a PHC string for plain.
//
// A fresh random salt per call means two clips with the same password do not
// share a hash, so the stored value cannot be used to tell that two clips have
// the same password (nor to spot-reuse one rainbow table across the table).
func hashPassword(plain string) (string, error) {
	salt := make([]byte, argonSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("read salt: %w", err)
	}
	key := argon2.IDKey([]byte(plain), salt, argonTime, argonMemoryKiB, argonParallel, argonKeyBytes)
	return encodeArgon2id(salt, key), nil
}

// verifyPassword reports whether plain matches the stored PHC string.
//
// It returns false — never an error — for a malformed stored value: a row that
// cannot be parsed must deny access, not crash the handler or (worse) be treated
// as "no password". Failing closed here is what keeps a corrupt row from
// silently becoming a public clip.
func verifyPassword(stored, plain string) bool {
	params, salt, want, err := decodeArgon2id(stored)
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(plain), salt, params.time, params.memory, params.parallel, uint32(len(want)))
	// Constant time: a length-dependent early return or a short-circuiting
	// comparison leaks how much of the hash matched, which over many attempts is
	// a byte-at-a-time oracle.
	return subtle.ConstantTimeCompare(got, want) == 1
}

type argonParams struct {
	memory   uint32
	time     uint32
	parallel uint8
}

func encodeArgon2id(salt, key []byte) string {
	// RawStdEncoding (no padding) is what the reference implementation uses;
	// padding characters are not part of the PHC grammar.
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemoryKiB, argonTime, argonParallel,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	)
}

// decodeArgon2id parses the subset of the PHC grammar this service writes.
//
// It is deliberately strict about the algorithm and version: accepting any
// algorithm named in the string would let a stored value decide how (or whether)
// verification happens.
func decodeArgon2id(encoded string) (argonParams, []byte, []byte, error) {
	var p argonParams
	parts := strings.Split(encoded, "$")
	// ["", "argon2id", "v=19", "m=..,t=..,p=..", salt, hash]
	if len(parts) != 6 || parts[0] != "" {
		return p, nil, nil, errors.New("not a PHC string")
	}
	if parts[1] != "argon2id" {
		return p, nil, nil, fmt.Errorf("unsupported algorithm %q", parts[1])
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return p, nil, nil, fmt.Errorf("bad version: %w", err)
	}
	if version != argon2.Version {
		return p, nil, nil, fmt.Errorf("unsupported argon2 version %d", version)
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.time, &p.parallel); err != nil {
		return p, nil, nil, fmt.Errorf("bad parameters: %w", err)
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return p, nil, nil, fmt.Errorf("bad salt: %w", err)
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return p, nil, nil, fmt.Errorf("bad hash: %w", err)
	}
	if len(salt) == 0 || len(key) == 0 {
		return p, nil, nil, errors.New("empty salt or hash")
	}
	return p, salt, key, nil
}
