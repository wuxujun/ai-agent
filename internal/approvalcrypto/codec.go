package approvalcrypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

const payloadVersion byte = 1

// Codec seals durable approval payloads with AES-256-GCM. The random nonce is
// prefixed by a one-byte format version so future key rotation formats can be
// introduced without confusing old records.
type Codec struct {
	aead cipher.AEAD
}

func New(key []byte) (*Codec, error) {
	if len(key) != 32 {
		return nil, errors.New("approval encryption key must be exactly 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create approval cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create approval AEAD: %w", err)
	}
	return &Codec{aead: aead}, nil
}

func NewFromBase64(encoded string) (*Codec, error) {
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		key, err = base64.RawStdEncoding.DecodeString(encoded)
	}
	if err != nil {
		return nil, errors.New("approval encryption key must be valid base64")
	}
	return New(key)
}

func (c *Codec) Encrypt(plaintext []byte) ([]byte, error) {
	if c == nil || c.aead == nil {
		return nil, errors.New("approval encryption codec is not configured")
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate approval nonce: %w", err)
	}
	result := make([]byte, 1, 1+len(nonce)+len(plaintext)+c.aead.Overhead())
	result[0] = payloadVersion
	result = append(result, nonce...)
	result = c.aead.Seal(result, nonce, plaintext, nil)
	return result, nil
}

func (c *Codec) Decrypt(payload []byte) ([]byte, error) {
	if c == nil || c.aead == nil {
		return nil, errors.New("approval encryption codec is not configured")
	}
	if len(payload) < 1+c.aead.NonceSize()+c.aead.Overhead() || payload[0] != payloadVersion {
		return nil, errors.New("invalid approval payload format")
	}
	nonceEnd := 1 + c.aead.NonceSize()
	plaintext, err := c.aead.Open(nil, payload[1:nonceEnd], payload[nonceEnd:], nil)
	if err != nil {
		return nil, errors.New("decrypt approval payload: authentication failed")
	}
	return plaintext, nil
}
