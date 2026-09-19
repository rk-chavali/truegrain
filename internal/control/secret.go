package control

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Encryption for the one thing this database holds that is worth stealing.
//
// The control plane stores warehouse credentials, which relaxes a rule this
// project wrote down for itself: "warehouse credentials stay in the
// environment; a control plane storing them in its own database is how this
// becomes a product that leaks somebody else's warehouse." That rule was
// right about the risk and wrong about the tradeoff, because a control
// plane nobody can connect a warehouse from is a control plane nobody uses.
// Metabase, Superset and Airbyte all made the same call.
//
// What makes it defensible is that the database alone is not enough. The key
// lives in the environment and never in the database, so a dumped database,
// a leaked backup, or a stolen replica is ciphertext. The two have to be
// stolen from two different places.
//
// AES-256-GCM, which authenticates as well as encrypts: a modified
// ciphertext fails to open rather than decrypting to something else. The
// nonce is random per encryption and stored with the ciphertext, because a
// reused nonce under the same key is the one mistake that breaks GCM
// completely.
//
// There is no "encryption disabled" mode and no default key. A default key
// is not encryption, it is a rot13 that reads as a security feature in an
// audit.

// KeyEnv names the environment variable holding the encryption key.
const KeyEnv = "TRUEGRAIN_CONTROL_KEY"

// Cipher encrypts and decrypts stored secrets.
type Cipher struct{ aead cipher.AEAD }

// NewCipher derives the cipher from a key.
//
// The key is any string with enough entropy; it is hashed to 32 bytes rather
// than required to be exactly 32 bytes of base64, because the alternative is
// an operator base64-encoding a short passphrase and believing they have a
// 256-bit key. Hashing does not create entropy, so the length floor below
// still applies and is what actually protects this.
func NewCipher(key string) (*Cipher, error) {
	if len(key) < 32 {
		return nil, fmt.Errorf(
			"%s must be at least 32 characters. Generate one with:\n"+
				"  openssl rand -base64 32", KeyEnv)
	}
	sum := sha256.Sum256([]byte(key))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: aead}, nil
}

// CipherFromEnv reads the key from the environment, failing closed.
//
// Deliberately an error rather than a warning and a fallback. A process that
// started without a key would either store credentials in the clear or
// refuse every connection later, and the first is a silent disaster while
// the second is the same failure discovered at the worst moment.
func CipherFromEnv() (*Cipher, error) {
	key := os.Getenv(KeyEnv)
	if strings.TrimSpace(key) == "" {
		return nil, fmt.Errorf(
			"%s is not set. The control plane stores warehouse credentials "+
				"encrypted and will not start without a key, because the "+
				"alternative is storing them in the clear. Generate one with:\n"+
				"  openssl rand -base64 32", KeyEnv)
	}
	return NewCipher(key)
}

// Seal encrypts a secret for storage.
func (c *Cipher) Seal(plaintext string) (string, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("generating a nonce: %w", err)
	}
	sealed := c.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Open decrypts a stored secret.
//
// A failure here almost always means the key changed rather than that the
// data is corrupt, so the message says so: an operator who rotated
// TRUEGRAIN_CONTROL_KEY without re-encrypting has a recoverable problem and
// needs to be told which one it is.
func (c *Cipher) Open(stored string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(stored)
	if err != nil {
		return "", errors.New("the stored secret is not valid base64")
	}
	if len(raw) < c.aead.NonceSize() {
		return "", errors.New("the stored secret is too short to be valid")
	}
	nonce, ciphertext := raw[:c.aead.NonceSize()], raw[c.aead.NonceSize():]
	plaintext, err := c.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf(
			"this secret cannot be decrypted with the current %s. If the key "+
				"was changed, the stored credentials were encrypted with the "+
				"previous one and have to be re-entered", KeyEnv)
	}
	return string(plaintext), nil
}
