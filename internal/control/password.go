package control

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Password hashing.
//
// argon2id rather than bcrypt. bcrypt is not broken, but it caps the
// password at 72 bytes, has no memory hardness, and a GPU farm is very good
// at it. argon2id is the current answer and is in golang.org/x/crypto,
// which this module already depends on.
//
// The encoded form carries its own parameters, so raising the cost later
// does not invalidate existing hashes: an old hash verifies with the
// parameters it was written with, and is rewritten on the next successful
// login. That upgrade path is the reason the parameters are stored rather
// than assumed.
//
// Nothing in this file logs, returns or formats a password. The only
// value that leaves is the encoded hash.

// Argon2 parameters.
//
// 64 MiB and one pass over it, with four lanes. This is the "second
// recommended option" from RFC 9106, chosen over the 2 GiB first option
// because the control plane is expected to run in a container somebody
// gave 512 MiB, and a login that OOMs is a login that does not happen.
// Measured at roughly 50ms on an ordinary core, which is the right order:
// slow enough to matter offline, fast enough that a human notices nothing.
const (
	argonTime    = 1
	argonMemory  = 64 * 1024 // KiB
	argonThreads = 4
	argonKeyLen  = 32
	argonSaltLen = 16
)

// MinPasswordLength is the floor.
//
// Twelve rather than eight. Composition rules, the ones demanding a symbol
// and a digit, are worse than useless: they push people to Passw0rd! and
// they are what NIST 800-63B now tells you not to do. Length is the only
// requirement that actually buys anything, so it is the only one here.
const MinPasswordLength = 12

// HashPassword returns the encoded argon2id hash of a password.
func HashPassword(password string) (string, error) {
	if err := CheckPasswordPolicy(password); err != nil {
		return "", err
	}
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generating a salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)

	// The standard PHC string format, so the hash is recognisable and
	// carries the parameters it was made with.
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// CheckPasswordPolicy enforces the one rule worth enforcing.
func CheckPasswordPolicy(password string) error {
	if len([]rune(password)) < MinPasswordLength {
		return fmt.Errorf("a password must be at least %d characters", MinPasswordLength)
	}
	// An upper bound, because argon2 will happily hash a megabyte and that
	// is a denial of service somebody can trigger from a signup form.
	if len(password) > 1024 {
		return errors.New("a password must be at most 1024 bytes")
	}
	return nil
}

// VerifyPassword reports whether a password matches an encoded hash.
//
// Constant time on the comparison, and the parameters come from the stored
// hash rather than from the constants above, so a hash written under older
// settings still verifies.
func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false
	}
	var memory uint32
	var time uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &threads); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}

	got := argon2.IDKey([]byte(password), salt, time, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// dummyHash is verified against when no account matches, so that a login
// attempt for an address that does not exist costs the same as one for an
// address that does.
//
// Without this, an unknown email returns in microseconds and a known one
// takes fifty milliseconds, which turns the login form into an endpoint for
// enumerating who has an account. The generic error message alone does not
// close that; the clock does.
var dummyHash = func() string {
	h, err := HashPassword("truegrain-timing-equaliser-not-a-real-password")
	if err != nil {
		panic(err) // A constant that fails the policy is a programming error.
	}
	return h
}()

// EqualiseTiming burns the same work a real verification would, for a login
// attempt that has already failed to find an account.
func EqualiseTiming(password string) { _ = VerifyPassword(dummyHash, password) }
