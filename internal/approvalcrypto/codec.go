package approvalcrypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	payloadVersionV1 byte = 1
	payloadVersionV2 byte = 2
	keyIDSize             = 8
)

// Codec seals durable approval payloads with AES-256-GCM. The random nonce is
// prefixed by a one-byte format version so future key rotation formats can be
// introduced without confusing old records.
type Codec struct {
	primaryID [keyIDSize]byte
	primary   cipher.AEAD
	byID      map[[keyIDSize]byte]cipher.AEAD
	legacy    []cipher.AEAD
}

func New(key []byte) (*Codec, error) {
	return NewKeyring(key)
}

func newAEAD(key []byte) (cipher.AEAD, error) {
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
	return aead, nil
}

func keyID(key []byte) [keyIDSize]byte {
	digest := sha256.Sum256(key)
	var id [keyIDSize]byte
	copy(id[:], digest[:keyIDSize])
	return id
}

// NewKeyring uses the first key for encryption and all keys for decryption.
func NewKeyring(primaryKey []byte, previousKeys ...[]byte) (*Codec, error) {
	keys := append([][]byte{primaryKey}, previousKeys...)
	codec := &Codec{byID: make(map[[keyIDSize]byte]cipher.AEAD)}
	for i, key := range keys {
		aead, err := newAEAD(key)
		if err != nil {
			return nil, err
		}
		id := keyID(key)
		if _, exists := codec.byID[id]; exists {
			continue
		}
		codec.byID[id] = aead
		codec.legacy = append(codec.legacy, aead)
		if i == 0 {
			codec.primaryID = id
			codec.primary = aead
		}
	}
	return codec, nil
}

func NewFromBase64(encoded string) (*Codec, error) {
	return NewFromBase64Keyring(encoded, nil)
}

func decodeBase64Key(encoded string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		key, err = base64.RawStdEncoding.DecodeString(strings.TrimSpace(encoded))
	}
	if err != nil {
		return nil, errors.New("approval encryption key must be valid base64")
	}
	return key, nil
}

func NewFromBase64Keyring(primary string, previous []string) (*Codec, error) {
	primaryKey, err := decodeBase64Key(primary)
	if err != nil {
		return nil, err
	}
	previousKeys := make([][]byte, 0, len(previous))
	for _, encoded := range previous {
		if strings.TrimSpace(encoded) == "" {
			continue
		}
		key, decodeErr := decodeBase64Key(encoded)
		if decodeErr != nil {
			return nil, fmt.Errorf("decode previous approval encryption key: %w", decodeErr)
		}
		previousKeys = append(previousKeys, key)
	}
	return NewKeyring(primaryKey, previousKeys...)
}

func (c *Codec) Encrypt(plaintext []byte) ([]byte, error) {
	if c == nil || c.primary == nil {
		return nil, errors.New("approval encryption codec is not configured")
	}
	nonce := make([]byte, c.primary.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate approval nonce: %w", err)
	}
	result := make([]byte, 1, 1+keyIDSize+len(nonce)+len(plaintext)+c.primary.Overhead())
	result[0] = payloadVersionV2
	result = append(result, c.primaryID[:]...)
	result = append(result, nonce...)
	result = c.primary.Seal(result, nonce, plaintext, nil)
	return result, nil
}

func (c *Codec) Decrypt(payload []byte) ([]byte, error) {
	if c == nil || c.primary == nil {
		return nil, errors.New("approval encryption codec is not configured")
	}
	switch payload[0] {
	case payloadVersionV1:
		for _, aead := range c.legacy {
			if plaintext, ok := openPayload(aead, payload, 1); ok {
				return plaintext, nil
			}
		}
	case payloadVersionV2:
		if len(payload) < 1+keyIDSize {
			return nil, errors.New("invalid approval payload format")
		}
		var id [keyIDSize]byte
		copy(id[:], payload[1:1+keyIDSize])
		if aead := c.byID[id]; aead != nil {
			if plaintext, ok := openPayload(aead, payload, 1+keyIDSize); ok {
				return plaintext, nil
			}
		}
	default:
		return nil, errors.New("invalid approval payload format")
	}
	return nil, errors.New("decrypt approval payload: authentication failed")
}

func openPayload(aead cipher.AEAD, payload []byte, offset int) ([]byte, bool) {
	nonceEnd := offset + aead.NonceSize()
	if offset < 0 || nonceEnd+aead.Overhead() > len(payload) {
		return nil, false
	}
	plaintext, err := aead.Open(nil, payload[offset:nonceEnd], payload[nonceEnd:], nil)
	return plaintext, err == nil
}
