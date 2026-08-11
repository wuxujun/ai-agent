package approvalcrypto

import (
	"bytes"
	"encoding/base64"
	"testing"
)

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
