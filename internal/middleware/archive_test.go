package middleware

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"testing"

	"golang.org/x/crypto/nacl/box"
)

func TestNewArchiveFromConfig_InvalidNaClKey(t *testing.T) {
	dispatcher := &ArchiveDispatcher{} // minimal, just needs non-nil

	tests := []struct {
		name string
		key  string
	}{
		{"bad base64", "not-valid!!!"},
		{"wrong length", base64.StdEncoding.EncodeToString([]byte("tooshort"))},
		{"truncated key", base64.StdEncoding.EncodeToString(make([]byte, 16))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := ArchiveConfig{
				URL:              "https://example.com/archive",
				Include:          "both",
				NaClRecipientKey: tt.key,
			}
			cfgJSON, _ := json.Marshal(cfg)
			_, err := NewArchiveFromConfig(cfgJSON, nil, dispatcher)
			if err == nil {
				t.Fatalf("expected error for key %q, got nil", tt.key)
			}
		})
	}
}

func TestNewArchiveFromConfig_ValidNaClKey(t *testing.T) {
	dispatcher := &ArchiveDispatcher{}
	pub, _, _ := box.GenerateKey(rand.Reader)
	b64Key := base64.StdEncoding.EncodeToString(pub[:])

	cfg := ArchiveConfig{
		URL:              "https://example.com/archive",
		Include:          "both",
		NaClRecipientKey: b64Key,
	}
	cfgJSON, _ := json.Marshal(cfg)
	mw, err := NewArchiveFromConfig(cfgJSON, nil, dispatcher)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	archive := mw.(*Archive)
	if archive.recipientKey == nil {
		t.Fatal("recipientKey should be cached, got nil")
	}
	if *archive.recipientKey != *pub {
		t.Error("cached key does not match original")
	}
}

func TestNewArchiveFromConfig_NoEncryption(t *testing.T) {
	dispatcher := &ArchiveDispatcher{}

	cfg := ArchiveConfig{
		URL:     "https://example.com/archive",
		Include: "both",
	}
	cfgJSON, _ := json.Marshal(cfg)
	mw, err := NewArchiveFromConfig(cfgJSON, nil, dispatcher)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	archive := mw.(*Archive)
	if archive.recipientKey != nil {
		t.Error("recipientKey should be nil when encryption is disabled")
	}
}

func TestArchive_CachedKeyUsedForEncryption(t *testing.T) {
	// Verify that the cached recipientKey matches what was configured
	dispatcher := &ArchiveDispatcher{}

	pub, _, _ := box.GenerateKey(rand.Reader)
	b64Key := base64.StdEncoding.EncodeToString(pub[:])

	cfg := ArchiveConfig{
		URL:              "https://example.com/archive",
		Include:          "both",
		NaClRecipientKey: b64Key,
	}
	cfgJSON, _ := json.Marshal(cfg)
	mw, err := NewArchiveFromConfig(cfgJSON, nil, dispatcher)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	archive := mw.(*Archive)

	// The cached key should allow encryption without re-decoding
	payload := []byte(`{"test": true}`)
	encrypted, err := encryptPayload(payload, *archive.recipientKey)
	if err != nil {
		t.Fatalf("encryption with cached key failed: %v", err)
	}
	if len(encrypted) == 0 {
		t.Error("encrypted payload is empty")
	}
}
