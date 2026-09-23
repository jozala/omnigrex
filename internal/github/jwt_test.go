package github_test

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"strings"
	"testing"
	"time"

	githubapi "github.com/jozala/omnigrex/internal/github"
)

type fixedClock struct {
	now time.Time
}

func (clock fixedClock) Now() time.Time {
	return clock.now
}

func TestAppJWTSignerSignsGitHubClaimsWithPKCS1AndPKCS8Keys(t *testing.T) {
	now := time.Date(2026, time.September, 2, 12, 30, 0, 0, time.UTC)
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatalf("marshal PKCS#8 key: %v", err)
	}

	tests := map[string][]byte{
		"PKCS#1": pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey)}),
		"PKCS#8": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}),
	}
	for name, keyPEM := range tests {
		t.Run(name, func(t *testing.T) {
			signer, err := githubapi.NewAppJWTSigner(123456, keyPEM, fixedClock{now: now})
			if err != nil {
				t.Fatalf("NewAppJWTSigner() error = %v", err)
			}
			token, err := signer.AppJWT(context.Background())
			if err != nil {
				t.Fatalf("AppJWT() error = %v", err)
			}

			parts := strings.Split(token, ".")
			if len(parts) != 3 {
				t.Fatalf("JWT has %d parts, want 3", len(parts))
			}
			var header map[string]string
			decodeJWTPart(t, parts[0], &header)
			if header["alg"] != "RS256" || header["typ"] != "JWT" {
				t.Errorf("JWT header = %#v, want RS256 JWT", header)
			}
			var claims struct {
				Issuer   int64 `json:"iss"`
				IssuedAt int64 `json:"iat"`
				Expires  int64 `json:"exp"`
			}
			decodeJWTPart(t, parts[1], &claims)
			if claims.Issuer != 123456 {
				t.Errorf("issuer = %d, want 123456", claims.Issuer)
			}
			if claims.IssuedAt != now.Add(-time.Minute).Unix() {
				t.Errorf("issued-at = %d, want %d", claims.IssuedAt, now.Add(-time.Minute).Unix())
			}
			if claims.Expires != now.Add(9*time.Minute).Unix() {
				t.Errorf("expiry = %d, want %d", claims.Expires, now.Add(9*time.Minute).Unix())
			}

			digest := crypto.SHA256.New()
			_, _ = digest.Write([]byte(parts[0] + "." + parts[1]))
			signature, err := base64.RawURLEncoding.DecodeString(parts[2])
			if err != nil {
				t.Fatalf("decode JWT signature: %v", err)
			}
			if err := rsa.VerifyPKCS1v15(&privateKey.PublicKey, crypto.SHA256, digest.Sum(nil), signature); err != nil {
				t.Errorf("verify JWT signature: %v", err)
			}
		})
	}
}

func TestAppJWTSignerRejectsInvalidConfiguration(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	validPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey)})

	for _, appID := range []int64{0, -1} {
		if _, err := githubapi.NewAppJWTSigner(appID, validPEM, fixedClock{}); !errors.Is(err, githubapi.ErrInvalidAppID) {
			t.Errorf("NewAppJWTSigner(%d) error = %v, want ErrInvalidAppID", appID, err)
		} else {
			var configuration *githubapi.ConfigurationError
			if !errors.As(err, &configuration) || !configuration.Permanent() {
				t.Errorf("NewAppJWTSigner(%d) error = %T %v, want permanent ConfigurationError", appID, err, err)
			}
		}
	}

	ecdsaKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ECDSA key: %v", err)
	}
	ecdsaPKCS8, err := x509.MarshalPKCS8PrivateKey(ecdsaKey)
	if err != nil {
		t.Fatalf("marshal ECDSA key: %v", err)
	}
	invalidKeys := [][]byte{
		[]byte("not PEM"),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("not DER")}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecdsaPKCS8}),
	}
	for _, keyPEM := range invalidKeys {
		if _, err := githubapi.NewAppJWTSigner(1, keyPEM, fixedClock{}); !errors.Is(err, githubapi.ErrInvalidPrivateKey) {
			t.Errorf("NewAppJWTSigner() error = %v, want ErrInvalidPrivateKey", err)
		}
	}
}

func TestAppJWTSignerPublicKeyFingerprintIdentifiesKeyReuse(t *testing.T) {
	firstKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate first RSA key: %v", err)
	}
	secondKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate second RSA key: %v", err)
	}
	firstPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(firstKey)})
	secondPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(secondKey)})
	first, err := githubapi.NewAppJWTSigner(1, firstPEM, fixedClock{})
	if err != nil {
		t.Fatalf("NewAppJWTSigner(first) error = %v", err)
	}
	reused, err := githubapi.NewAppJWTSigner(2, firstPEM, fixedClock{})
	if err != nil {
		t.Fatalf("NewAppJWTSigner(reused) error = %v", err)
	}
	second, err := githubapi.NewAppJWTSigner(2, secondPEM, fixedClock{})
	if err != nil {
		t.Fatalf("NewAppJWTSigner(second) error = %v", err)
	}
	if first.PublicKeyFingerprint() != reused.PublicKeyFingerprint() {
		t.Error("same RSA key produced different public-key fingerprints")
	}
	if first.PublicKeyFingerprint() == second.PublicKeyFingerprint() {
		t.Error("different RSA keys produced the same public-key fingerprint")
	}
}

func decodeJWTPart(t *testing.T, part string, destination any) {
	t.Helper()
	decoded, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		t.Fatalf("decode JWT part: %v", err)
	}
	if err := json.Unmarshal(decoded, destination); err != nil {
		t.Fatalf("unmarshal JWT part: %v", err)
	}
}
