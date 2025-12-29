package scaler

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func computeSignature(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestValidateSignature_ValidSignature(t *testing.T) {
	secret := []byte("test-secret")
	body := []byte(`{"action":"queued"}`)
	signature := computeSignature(secret, body)

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", signature)

	if !ValidateSignature(secret, req) {
		t.Error("expected valid signature to pass")
	}

	// Verify body is restored for subsequent reads
	restoredBody, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("failed to read restored body: %v", err)
	}
	if !bytes.Equal(restoredBody, body) {
		t.Errorf("body not restored correctly: got %q, want %q", restoredBody, body)
	}
}

func TestValidateSignature_InvalidSignature(t *testing.T) {
	secret := []byte("test-secret")
	body := []byte(`{"action":"queued"}`)
	wrongSignature := computeSignature([]byte("wrong-secret"), body)

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", wrongSignature)

	if ValidateSignature(secret, req) {
		t.Error("expected invalid signature to fail")
	}
}

func TestValidateSignature_MissingSignature(t *testing.T) {
	secret := []byte("test-secret")
	body := []byte(`{"action":"queued"}`)

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))

	if ValidateSignature(secret, req) {
		t.Error("expected missing signature to fail")
	}
}

func TestValidateSignature_WrongPrefix(t *testing.T) {
	secret := []byte("test-secret")
	body := []byte(`{"action":"queued"}`)

	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	wrongPrefixSig := "sha1=" + hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", wrongPrefixSig)

	if ValidateSignature(secret, req) {
		t.Error("expected wrong prefix to fail")
	}
}

func TestValidateSignature_EmptyBody(t *testing.T) {
	secret := []byte("test-secret")
	body := []byte{}
	signature := computeSignature(secret, body)

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", signature)

	if !ValidateSignature(secret, req) {
		t.Error("expected valid signature with empty body to pass")
	}
}

func TestValidateSignature_TamperedBody(t *testing.T) {
	secret := []byte("test-secret")
	originalBody := []byte(`{"action":"queued"}`)
	tamperedBody := []byte(`{"action":"completed"}`)
	signature := computeSignature(secret, originalBody)

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(tamperedBody))
	req.Header.Set("X-Hub-Signature-256", signature)

	if ValidateSignature(secret, req) {
		t.Error("expected tampered body to fail signature validation")
	}
}
