// Package auth holds the credential primitives: password hashing (argon2id) and opaque tokens (sessions, email links).
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters (OWASP 2024 baseline: m=19 MiB, t=2, p=1). Stored in the hash, so they can be raised later
// without invalidating existing passwords; NeedsRehash reports hashes made with weaker settings.
const (
	argonMemory  = 19 * 1024
	argonTime    = 2
	argonThreads = 1
	argonKeyLen  = 32
	saltLen      = 16
)

const MinPasswordLength = 8

// HashPassword returns a PHC-format argon2id hash: $argon2id$v=19$m=…,t=…,p=…$salt$key.
func HashPassword(password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads,
		b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

var errMalformed = errors.New("malformed password hash")

// VerifyPassword reports whether password matches hash, in constant time.
func VerifyPassword(password, hash string) (bool, error) {
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, errMalformed
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, errMalformed
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false, errMalformed
	}
	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return false, errMalformed
	}
	want, err := b64.DecodeString(parts[5])
	if err != nil {
		return false, errMalformed
	}
	got := argon2.IDKey([]byte(password), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// dummyHash is verified against when the account does not exist, so a login for an unknown email takes as long as
// one for a known email (no account enumeration by timing).
var dummyHash, _ = HashPassword("ecopurnity-timing-equaliser")

// BurnTime spends the same work as VerifyPassword on a real hash.
func BurnTime(password string) { _, _ = VerifyPassword(password, dummyHash) }
