package approvalcrypto

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"testing"
)

func sealLegacyV1(t *testing.T, key, plaintext []byte) []byte {
	t.Helper()
	aead, err := newAEAD(key)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	payload := append([]byte{payloadVersionV1}, nonce...)
	return aead.Seal(payload, nonce, plaintext, nil)
}

func TestCodecRoundTripAndTamperDetection(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	codec, err := NewFromBase64(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	plaintext := []byte(`{"action":"write_file","parameters":{"content":"secret"}}`)
	sealed, err := codec.Encrypt(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("secret")) {
		t.Fatal("ciphertext contains plaintext")
	}
	opened, err := codec.Decrypt(sealed)
	if err != nil || !bytes.Equal(opened, plaintext) {
		t.Fatalf("Decrypt = %q, %v", opened, err)
	}
	sealed[len(sealed)-1] ^= 1
	if _, err := codec.Decrypt(sealed); err == nil {
		t.Fatal("tampered payload decrypted successfully")
	}
}

func TestCodecRejectsWrongKeyLength(t *testing.T) {
	if _, err := New([]byte("short")); err == nil {
		t.Fatal("New accepted a short key")
	}
}

func TestCodecKeyRotationReadsPreviousV1AndV2(t *testing.T) {
	oldKey := bytes.Repeat([]byte{0x11}, 32)
	newKey := bytes.Repeat([]byte{0x22}, 32)
	plaintext := []byte("rotation-checkpoint")
	oldCodec, err := New(oldKey)
	if err != nil {
		t.Fatal(err)
	}
	oldV2, err := oldCodec.Encrypt(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := NewKeyring(newKey, oldKey)
	if err != nil {
		t.Fatal(err)
	}
	for name, payload := range map[string][]byte{"v1": sealLegacyV1(t, oldKey, plaintext), "v2": oldV2} {
		opened, decryptErr := rotated.Decrypt(payload)
		if decryptErr != nil || !bytes.Equal(opened, plaintext) {
			t.Fatalf("decrypt previous %s = %q, %v", name, opened, decryptErr)
		}
	}
	newPayload, err := rotated.Encrypt(plaintext)
	if err != nil || len(newPayload) == 0 || newPayload[0] != payloadVersionV2 {
		t.Fatalf("new payload version = %v, %v", newPayload, err)
	}
	if _, err := oldCodec.Decrypt(newPayload); err == nil {
		t.Fatal("old key decrypted payload written with new primary")
	}
}

func TestCodecKeyringRejectsInvalidPreviousKey(t *testing.T) {
	primary := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x33}, 32))
	if _, err := NewFromBase64Keyring(primary, []string{"invalid"}); err == nil {
		t.Fatal("accepted invalid previous key")
	}
}
