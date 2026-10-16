package config

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/crypto/pbkdf2"
)

// Encryption of the configuration at rest.
//
// This exists for one specific threat: the file outliving the process. In a
// container the configuration sits on a persistent volume so it survives a
// restart, and that volume gets snapshotted, backed up and copied. Encrypting
// the file means a stray snapshot is not a list of database credentials.
//
// It is deliberately not the first line of defence. The better answer, and
// the documented default, is to keep passwords out of the file altogether by
// writing ${PGPASSWORD} in the connection string and supplying that variable
// from a secret at run time. Then the volume holds no secret to protect. Use
// encryption as well when connection strings must carry their own passwords.
//
// The key never touches disk: it comes from the IDXA_CONFIG_KEY environment
// variable, which a container runtime populates from its own secret store.

const (
	// keyEnv is where the key comes from.
	keyEnv = "IDXA_CONFIG_KEY"

	// magic marks an encrypted file so a plaintext one can still be read,
	// which lets an existing installation turn encryption on without a
	// migration step.
	magic = "idxa-encrypted-v1\n"

	saltLen  = 16
	nonceLen = 12
	// iterations for the key derivation. High enough to make a weak
	// passphrase expensive to attack, low enough not to delay startup.
	iterations = 600_000
)

// ErrKeyRequired says the file is encrypted but no key was supplied.
var ErrKeyRequired = errors.New(
	"this configuration is encrypted; set " + keyEnv + " to the key it was written with")

// ErrWrongKey says the key did not decrypt the file.
var ErrWrongKey = errors.New(
	"the key in " + keyEnv + " does not decrypt this configuration")

// encryptionKey returns the configured key material, or nil when encryption
// is off.
func encryptionKey() []byte {
	value := os.Getenv(keyEnv)
	if value == "" {
		return nil
	}
	return []byte(value)
}

// isEncrypted reports whether data carries the encrypted envelope.
func isEncrypted(data []byte) bool {
	return len(data) > len(magic) && string(data[:len(magic)]) == magic
}

// deriveKey turns the supplied secret into a 32 byte key.
//
// A caller may provide a proper 32 byte key encoded as hex or base64, in
// which case it is used directly. Anything else is treated as a passphrase
// and stretched, so a short or guessable value is still expensive to attack.
func deriveKey(secret, salt []byte) []byte {
	if raw, err := hex.DecodeString(string(secret)); err == nil && len(raw) == 32 {
		return raw
	}
	if raw, err := base64.StdEncoding.DecodeString(string(secret)); err == nil && len(raw) == 32 {
		return raw
	}
	return pbkdf2.Key(secret, salt, iterations, 32, sha256.New)
}

// encrypt wraps plaintext in the envelope: magic, salt, nonce, ciphertext.
func encrypt(plaintext, secret []byte) ([]byte, error) {
	salt := make([]byte, saltLen)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, fmt.Errorf("generate salt: %w", err)
	}

	block, err := aes.NewCipher(deriveKey(secret, salt))
	if err != nil {
		return nil, fmt.Errorf("build cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("build cipher mode: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}

	// The magic header is authenticated as additional data, so a file cannot
	// be silently downgraded by stripping it.
	sealed := gcm.Seal(nil, nonce, plaintext, []byte(magic))

	out := make([]byte, 0, len(magic)+len(salt)+len(nonce)+len(sealed))
	out = append(out, magic...)
	out = append(out, salt...)
	out = append(out, nonce...)
	out = append(out, sealed...)
	return out, nil
}

// decrypt unwraps an envelope.
func decrypt(data, secret []byte) ([]byte, error) {
	if !isEncrypted(data) {
		return nil, errors.New("not an encrypted configuration")
	}
	body := data[len(magic):]
	if len(body) < saltLen+nonceLen {
		return nil, errors.New("the encrypted configuration is truncated")
	}

	salt := body[:saltLen]
	rest := body[saltLen:]

	block, err := aes.NewCipher(deriveKey(secret, salt))
	if err != nil {
		return nil, fmt.Errorf("build cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("build cipher mode: %w", err)
	}
	if len(rest) < gcm.NonceSize() {
		return nil, errors.New("the encrypted configuration is truncated")
	}

	nonce := rest[:gcm.NonceSize()]
	sealed := rest[gcm.NonceSize():]

	plaintext, err := gcm.Open(nil, nonce, sealed, []byte(magic))
	if err != nil {
		// Authentication failure means the wrong key or a tampered file, and
		// the two are indistinguishable by design.
		return nil, ErrWrongKey
	}
	return plaintext, nil
}

// EncryptionEnabled reports whether a key is configured, so the interface can
// say plainly whether the file on disk is protected.
func EncryptionEnabled() bool { return encryptionKey() != nil }

// EncryptionStatus describes the state of the configuration file for display.
type EncryptionStatus struct {
	// Enabled is true when a key is configured.
	Enabled bool `json:"enabled"`
	// FileEncrypted is true when the file on disk is actually encrypted,
	// which can differ from Enabled if the key was added after the file was
	// written and nothing has been saved since.
	FileEncrypted bool `json:"file_encrypted"`
	// Advice explains what to do about any mismatch.
	Advice string `json:"advice,omitempty"`
}

// Status inspects the configuration file.
func (s *Store) Status() EncryptionStatus {
	st := EncryptionStatus{Enabled: EncryptionEnabled()}

	data, err := os.ReadFile(s.path)
	if err == nil {
		st.FileEncrypted = isEncrypted(data)
	}

	switch {
	case st.Enabled && !st.FileEncrypted && err == nil:
		st.Advice = "a key is set but the file on disk is still plaintext; it will be encrypted the next time anything is saved"
	case !st.Enabled && st.FileEncrypted:
		st.Advice = "the file is encrypted but no key is set"
	case !st.Enabled:
		st.Advice = "the configuration is stored in plaintext, readable only by the user running idxa. " +
			"Keep passwords out of it with ${VAR} references, or set " + keyEnv + " to encrypt it at rest"
	}
	return st
}
