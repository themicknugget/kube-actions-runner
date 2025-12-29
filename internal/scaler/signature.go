package scaler

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log"
	"net/http"
	"strings"
)

func ValidateSignature(secret []byte, r *http.Request) bool {
	sig := r.Header.Get("X-Hub-Signature-256")
	if sig == "" || !strings.HasPrefix(sig, "sha256=") {
		return false
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("ERROR: failed to read request body for signature validation: %v", err)
		return false
	}
	r.Body = io.NopCloser(bytes.NewBuffer(body))

	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(sig), []byte(expected))
}
