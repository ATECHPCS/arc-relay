package middleware

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"golang.org/x/crypto/nacl/box"
)

// naclEnvelope is the encrypted payload envelope sent to the archive webhook.
type naclEnvelope struct {
	Nonce           string `json:"nonce"`
	Ciphertext      string `json:"ciphertext"`
	SourcePublicKey string `json:"sourcePublicKey"`
}

// encryptPayload encrypts a JSON payload using NaCl Box (X25519 + XSalsa20-Poly1305)
// with an ephemeral sender keypair. Returns the JSON-encoded envelope.
func encryptPayload(payload []byte, recipientPubKey [32]byte) ([]byte, error) {
	senderPub, senderPriv, err := box.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating ephemeral keypair: %w", err)
	}

	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, fmt.Errorf("generating nonce: %w", err)
	}

	ciphertext := box.Seal(nil, payload, &nonce, &recipientPubKey, senderPriv)

	envelope := naclEnvelope{
		Nonce:           base64.StdEncoding.EncodeToString(nonce[:]),
		Ciphertext:      base64.StdEncoding.EncodeToString(ciphertext),
		SourcePublicKey: base64.StdEncoding.EncodeToString(senderPub[:]),
	}

	return json.Marshal(envelope)
}

// decodeRecipientKey decodes a base64-encoded Curve25519 public key.
func decodeRecipientKey(b64Key string) ([32]byte, error) {
	var key [32]byte
	raw, err := base64.StdEncoding.DecodeString(b64Key)
	if err != nil {
		return key, fmt.Errorf("invalid base64: %w", err)
	}
	if len(raw) != 32 {
		return key, fmt.Errorf("expected 32 bytes, got %d", len(raw))
	}
	copy(key[:], raw)
	return key, nil
}
