package clip

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zeroicey/clip/internal/clip/clipdb"
)

// Shares are the read-only second entrance to a clip, at `/s/<token>`.
//
// The whole design follows from one scenario, spelled out in
// .ai/decisions/2026-10-16-clip-share-links.md: `clip.hcyj.xyz/我的名字` is my own
// private note (often with a password), and I want a colleague to read one thing
// without giving them the master password and without letting them write. So the
// share entrance is:
//
//   - read-only at the route layer — `/s/...` only ever answers GET/HEAD, so
//     there is no write endpoint to hide behind a button;
//   - protected by its OWN secret, never by the clip's master password (that is
//     the entire point: do not spread the master password around);
//   - revocable, and expiring;
//   - pointing at the same row as the clip, so an edit by the owner shows up on
//     the share side with no copy to invalidate.
//
// A share carries at most one secret. A master-chosen password and a
// server-generated pickup code are two answers to the same question ("what does
// the visitor type?"), and a row that had both would have no defined way to
// decide which one the visitor meant.

const (
	// shareTokenBytes is 16 bytes → 22 base64url characters. High enough entropy
	// that the token cannot be guessed or enumerated (which is also what makes
	// answering "this link is dead" differently from "wrong secret" acceptable:
	// see ShareDecision).
	shareTokenBytes = 16
	// ShareTokenMinLen/ShareTokenMaxLen bound what this service will even look up.
	// They are deliberately wider than the 22 characters it writes, so a future
	// longer token still works, while a pathological input cannot be used to make
	// the database compare megabyte strings.
	ShareTokenMinLen = 16
	ShareTokenMaxLen = 64
	// ShareCodeDigits is the "pickup code" length — the same idea as a parcel
	// locker code. Ten to the sixth possibilities is enumerable *if* guesses are
	// free, which is why the code is only ever issued with a short TTL and is
	// charged against the per-token guess budget on every failure.
	ShareCodeDigits = 6
)

// ErrInvalidShareToken is returned by ValidateShareToken. The API layer maps it
// onto a 404, not a 400: a malformed token and an unknown token are the same
// thing to the visitor ("this link does not work"), and answering 400 would let
// someone distinguish "well-formed but unknown" from "not even the right shape"
// — a distinction with no user value and a little enumeration value.
var ErrInvalidShareToken = errors.New("invalid share token")

// Share is one share credential, as the store hands it to the API layer.
//
// PasswordHash and CodeHash never reach the wire: they are converted to the
// booleans `has_password` / `has_code`, exactly as Clip.PasswordHash is converted
// to `has_password`. They are *string so that nil ("no secret, the link is
// enough") cannot be confused with an empty value, which no PHC string ever is.
type Share struct {
	Token        string
	Suffix       string
	PasswordHash *string
	CodeHash     *string
	ExpiresAt    int64 // epoch ms; 0 = never
	CreatedAt    int64
}

// HasPassword reports whether the owner chose this share's secret themselves.
func (sh *Share) HasPassword() bool { return sh.PasswordHash != nil }

// HasCode reports whether this share carries a server-generated pickup code.
func (sh *Share) HasCode() bool { return sh.CodeHash != nil }

// NeedsSecret reports whether the visitor has to type anything at all. A share
// with neither is "anyone holding the link may read" — a plain read-only link.
func (sh *Share) NeedsSecret() bool { return sh.HasPassword() || sh.HasCode() }

// Verify decides whether the visitor's secret opens this share.
//
// It returns false when the share carries no secret at all: callers must ask
// NeedsSecret() first. That is not pedantry — a Verify that returned true for
// "no credential" would turn a caller's forgotten NeedsSecret() check into a
// silent authentication bypass, which is the failure this project treats as a
// bug rather than a tolerable default.
func (sh *Share) Verify(secret string) bool {
	switch {
	case sh.HasPassword():
		return verifyPassword(*sh.PasswordHash, secret)
	case sh.HasCode():
		return verifyPassword(*sh.CodeHash, secret)
	default:
		return false
	}
}

// newShareToken mints the path segment of a share link.
//
// RawURLEncoding for the same reason the CSP nonce uses it: the alphabet
// (`A-Za-z0-9-_`) is attribute-clean and URL-safe, so the token never needs
// escaping in a URL, an attribute or a clipboard.
func newShareToken() (string, error) {
	buf := make([]byte, shareTokenBytes)
	if _, err := readRandom(buf); err != nil {
		return "", fmt.Errorf("read share token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// ValidateShareToken rejects a token this service could never have issued.
func ValidateShareToken(token string) error {
	if len(token) < ShareTokenMinLen || len(token) > ShareTokenMaxLen {
		return ErrInvalidShareToken
	}
	for i := 0; i < len(token); i++ {
		c := token[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return ErrInvalidShareToken
		}
	}
	return nil
}

// newShareCode returns a uniformly random 6-digit pickup code.
//
// crypto/rand.Int with rejection (which is what Int does internally) rather than
// a modulo of a raw word: a biased code is a code whose low digits are more
// likely, and with only 10^6 possibilities even a small bias is a real
// reduction of the search space.
func newShareCode() (string, error) {
	const max = 1_000_000
	n, err := rand.Int(rand.Reader, big.NewInt(max))
	if err != nil {
		return "", fmt.Errorf("read share code: %w", err)
	}
	return fmt.Sprintf("%0*d", ShareCodeDigits, n.Int64()), nil
}

// ---------------------------------------------------------------------------
// store
// ---------------------------------------------------------------------------

// CreateShare inserts a share, and reports ErrNotFound when the caller's proof
// does not authorise it (wrong master password, no such clip, expired clip).
//
// The authorisation is part of the INSERT (see queries/shares.sql): `proof` is
// the stored master hash the caller already verified in Go, and the statement
// only inserts if the clip's hash still matches it. A password changed between
// the verification and this call therefore inserts nothing — the same
// one-statement rule every other write in this service follows, and the reason
// there is no read-then-write here either.
func (s *Store) CreateShare(ctx context.Context, suffix, proof, token string, passwordHash, codeHash *string, expiresAt int64) error {
	n, err := s.q.InsertShare(ctx, clipdb.InsertShareParams{
		Token:        token,
		Suffix:       suffix,
		PasswordHash: nullableText(passwordHash),
		CodeHash:     nullableText(codeHash),
		ExpiresAt:    expiresAt,
		NowMs:        s.nowMS(),
		ProofHash:    proofParam(proof),
	})
	if err != nil {
		// The foreign key (suffix → clips) can fail if the clip was deleted in the
		// instant between the handler reading it and this statement. That is not an
		// internal error: it is the same "there is nothing to share" answer.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return ErrNotFound
		}
		return fmt.Errorf("insert share: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetShare returns a live share (its own TTL has not passed).
//
// It deliberately does NOT consult the clip's master password: a share secret is
// independent of it, which is the whole point of the feature. Whether the share
// needs a secret, and whether the caller supplied it, is decided in the API layer.
func (s *Store) GetShare(ctx context.Context, token string) (*Share, error) {
	row, err := s.q.GetLiveShare(ctx, clipdb.GetLiveShareParams{Token: token, NowMs: s.nowMS()})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get share: %w", err)
	}
	return shareFromRow(row), nil
}

// ListShares returns every live or expired share of a clip, newest first.
//
// Expired rows are included on purpose: the owner's list is where they would go
// to revoke something, and silently hiding an expired share would make "why is
// this link still in my list" and "why did my link stop working" both harder to
// answer. The rows carry hashes, never plaintext, so the list leaks nothing.
func (s *Store) ListShares(ctx context.Context, suffix string) ([]Share, error) {
	rows, err := s.q.ListSharesForSuffix(ctx, suffix)
	if err != nil {
		return nil, fmt.Errorf("list shares: %w", err)
	}
	shares := make([]Share, 0, len(rows))
	for _, r := range rows {
		shares = append(shares, *shareFromRow(r))
	}
	return shares, nil
}

// DeleteShare revokes one share. `proof` is the clip's verified master hash, and
// it is part of the DELETE (see queries/shares.sql), so a master password that
// changed in between revokes nothing.
//
// The token is matched together with the suffix, so a token that belongs to
// another clip cannot be revoked through this clip's authority.
func (s *Store) DeleteShare(ctx context.Context, suffix, proof, token string) (bool, error) {
	n, err := s.q.DeleteShare(ctx, clipdb.DeleteShareParams{
		Token:     token,
		Suffix:    suffix,
		ProofHash: proofParam(proof),
	})
	if err != nil {
		return false, fmt.Errorf("delete share: %w", err)
	}
	return n > 0, nil
}

func shareFromRow(row clipdb.ClipShare) *Share {
	return &Share{
		Token:        row.Token,
		Suffix:       row.Suffix,
		PasswordHash: passwordPtr(row.PasswordHash),
		CodeHash:     passwordPtr(row.CodeHash),
		ExpiresAt:    row.ExpiresAt,
		CreatedAt:    row.CreatedAt,
	}
}
