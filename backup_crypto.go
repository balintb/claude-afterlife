package main

import (
	"bytes"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"filippo.io/age"
	"github.com/klauspost/compress/zstd"
)

var (
	errWrongKey = errors.New("this private key cannot open the backup")
	errTampered = errors.New("the backup index failed its integrity check: it was not written with this key, or it was modified")
)

var (
	codecOnce   sync.Once
	zstdEncoder *zstd.Encoder
	zstdDecoder *zstd.Decoder
)

func codecs() (*zstd.Encoder, *zstd.Decoder) {
	codecOnce.Do(func() {
		zstdEncoder, _ = zstd.NewWriter(nil)
		zstdDecoder, _ = zstd.NewReader(nil, zstd.WithDecoderMaxMemory(1<<30))
	})
	return zstdEncoder, zstdDecoder
}

// seal compresses and encrypts data for the given recipients. Only their private
// keys can open the result.
func seal(plain []byte, recipients []age.Recipient) ([]byte, error) {
	encoder, _ := codecs()
	var out bytes.Buffer
	writer, err := age.Encrypt(&out, recipients...)
	if err != nil {
		return nil, err
	}
	if _, err := writer.Write(encoder.EncodeAll(plain, nil)); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func unseal(sealed []byte, identities []age.Identity) ([]byte, error) {
	reader, err := age.Decrypt(bytes.NewReader(sealed), identities...)
	if err != nil {
		var noMatch *age.NoIdentityMatchError
		if errors.As(err, &noMatch) {
			return nil, errWrongKey
		}
		return nil, fmt.Errorf("decrypting: %w", err)
	}
	compressed, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("decrypting: %w", err)
	}
	_, decoder := codecs()
	plain, err := decoder.DecodeAll(compressed, nil)
	if err != nil {
		return nil, fmt.Errorf("decompressing: %w", err)
	}
	return plain, nil
}

// backupKey is a private key as pasted by the user, with what is derived from it.
type backupKey struct {
	identities []age.Identity
	secret     string
	recipient  string
}

func newBackupKey() (secret, recipient string, err error) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		return "", "", err
	}
	return identity.String(), identity.Recipient().String(), nil
}

func parseBackupKey(text string) (backupKey, error) {
	identities, err := age.ParseIdentities(strings.NewReader(text))
	if err != nil {
		return backupKey{}, errors.New("that is not a claude-afterlife private key (it starts with AGE-SECRET-KEY-1)")
	}
	for _, identity := range identities {
		if x, ok := identity.(*age.X25519Identity); ok {
			return backupKey{identities: identities, secret: x.String(), recipient: x.Recipient().String()}, nil
		}
	}
	return backupKey{}, errors.New("no AGE-SECRET-KEY-1 private key found")
}

func parseRecipients(values []string) ([]age.Recipient, error) {
	recipients := make([]age.Recipient, 0, len(values))
	for _, value := range values {
		recipient, err := age.ParseX25519Recipient(value)
		if err != nil {
			return nil, fmt.Errorf("invalid public key %q: %w", value, err)
		}
		recipients = append(recipients, recipient)
	}
	if len(recipients) == 0 {
		return nil, errors.New("no public key to encrypt to")
	}
	return recipients, nil
}

// macKeyFor derives the index authentication key from the private key. Encryption
// alone does not prove who wrote a backup, since anyone with the public key can
// encrypt; the MAC does, and it is derived so that the private key in the password
// manager is still the only thing needed to restore.
func macKeyFor(secret string) ([]byte, error) {
	return hkdf.Key(sha256.New, []byte(secret), []byte("claude-afterlife"), "index mac v1", 32)
}

func indexMAC(key, sealedIndex []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write(sealedIndex)
	return hex.EncodeToString(mac.Sum(nil))
}

func validMAC(key, sealedIndex []byte, mac string) bool {
	want, err := hex.DecodeString(strings.TrimSpace(mac))
	if err != nil {
		return false
	}
	computed := hmac.New(sha256.New, key)
	computed.Write(sealedIndex)
	return hmac.Equal(computed.Sum(nil), want)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
