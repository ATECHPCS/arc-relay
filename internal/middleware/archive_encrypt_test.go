package middleware

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"testing"

	"golang.org/x/crypto/nacl/box"
)

func TestEncryptPayloadRoundTrip(t *testing.T) {
	// Generate recipient keypair
	recipientPub, recipientPriv, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating recipient keypair: %v", err)
	}

	payload := []byte(`{"version":"v1","source":"arc_relay","phase":"test"}`)

	// Encrypt
	envelopeJSON, err := encryptPayload(payload, *recipientPub)
	if err != nil {
		t.Fatalf("encryptPayload: %v", err)
	}

	// Parse envelope
	var envelope naclEnvelope
	if err := json.Unmarshal(envelopeJSON, &envelope); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}

	// Verify all fields present
	if envelope.Nonce == "" {
		t.Error("nonce is empty")
	}
	if envelope.Ciphertext == "" {
		t.Error("ciphertext is empty")
	}
	if envelope.SourcePublicKey == "" {
		t.Error("sourcePublicKey is empty")
	}

	// Decode and decrypt
	nonce, err := base64.StdEncoding.DecodeString(envelope.Nonce)
	if err != nil {
		t.Fatalf("decode nonce: %v", err)
	}
	ciphertext, err := base64.StdEncoding.DecodeString(envelope.Ciphertext)
	if err != nil {
		t.Fatalf("decode ciphertext: %v", err)
	}
	senderPub, err := base64.StdEncoding.DecodeString(envelope.SourcePublicKey)
	if err != nil {
		t.Fatalf("decode sender pub: %v", err)
	}

	var nonceArr [24]byte
	copy(nonceArr[:], nonce)
	var senderPubArr [32]byte
	copy(senderPubArr[:], senderPub)

	decrypted, ok := box.Open(nil, ciphertext, &nonceArr, &senderPubArr, recipientPriv)
	if !ok {
		t.Fatal("box.Open failed - decryption error")
	}

	if string(decrypted) != string(payload) {
		t.Errorf("decrypted = %q, want %q", string(decrypted), string(payload))
	}
}

func TestDecodeRecipientKey(t *testing.T) {
	// Valid 32-byte key
	pub, _, _ := box.GenerateKey(rand.Reader)
	b64 := base64.StdEncoding.EncodeToString(pub[:])

	key, err := decodeRecipientKey(b64)
	if err != nil {
		t.Fatalf("decodeRecipientKey: %v", err)
	}
	if key != *pub {
		t.Error("decoded key does not match original")
	}

	// Invalid base64
	_, err = decodeRecipientKey("not-valid-base64!!!")
	if err == nil {
		t.Error("expected error for invalid base64")
	}

	// Wrong length
	short := base64.StdEncoding.EncodeToString([]byte("tooshort"))
	_, err = decodeRecipientKey(short)
	if err == nil {
		t.Error("expected error for wrong length key")
	}
}

func TestEncryptPayloadDifferentNonces(t *testing.T) {
	pub, _, _ := box.GenerateKey(rand.Reader)
	payload := []byte(`{"test": true}`)

	env1, _ := encryptPayload(payload, *pub)
	env2, _ := encryptPayload(payload, *pub)

	var e1, e2 naclEnvelope
	if err := json.Unmarshal(env1, &e1); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(env2, &e2); err != nil {
		t.Fatal(err)
	}

	if e1.Nonce == e2.Nonce {
		t.Error("two encryptions produced the same nonce - nonces must be unique")
	}
	if e1.SourcePublicKey == e2.SourcePublicKey {
		t.Error("two encryptions used the same ephemeral key - keys must be unique per message")
	}
}
