package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"time"
)

var (
	ErrInvalidAppID      = errors.New("GitHub App ID must be positive")
	ErrInvalidPrivateKey = errors.New("invalid GitHub App RSA private key")
)

type Clock interface {
	Now() time.Time
}

// PublicKeyFingerprint returns a stable SHA-256 identity for role-separation checks.
func (signer *AppJWTSigner) PublicKeyFingerprint() [sha256.Size]byte {
	return sha256.Sum256(x509.MarshalPKCS1PublicKey(&signer.privateKey.PublicKey))
}

type AppJWTProvider interface {
	AppJWT(context.Context) (string, error)
}

type AppJWTSigner struct {
	appID      int64
	privateKey *rsa.PrivateKey
	clock      Clock
}

func NewAppJWTSigner(appID int64, privateKeyPEM []byte, clock Clock) (*AppJWTSigner, error) {
	if appID <= 0 {
		return nil, &ConfigurationError{Cause: ErrInvalidAppID}
	}
	privateKey, err := parseRSAPrivateKey(privateKeyPEM)
	if err != nil {
		return nil, &ConfigurationError{Cause: err}
	}
	if clock == nil {
		clock = systemClock{}
	}
	return &AppJWTSigner{appID: appID, privateKey: privateKey, clock: clock}, nil
}

func (signer *AppJWTSigner) AppJWT(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	now := signer.clock.Now()
	header, err := json.Marshal(struct {
		Algorithm string `json:"alg"`
		Type      string `json:"typ"`
	}{Algorithm: "RS256", Type: "JWT"})
	if err != nil {
		return "", fmt.Errorf("marshal GitHub App JWT header: %w", err)
	}
	claims, err := json.Marshal(struct {
		Issuer   int64 `json:"iss"`
		IssuedAt int64 `json:"iat"`
		Expires  int64 `json:"exp"`
	}{
		Issuer:   signer.appID,
		IssuedAt: now.Add(-time.Minute).Unix(),
		Expires:  now.Add(9 * time.Minute).Unix(),
	})
	if err != nil {
		return "", fmt.Errorf("marshal GitHub App JWT claims: %w", err)
	}
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := crypto.SHA256.New()
	_, _ = digest.Write([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, signer.privateKey, crypto.SHA256, digest.Sum(nil))
	if err != nil {
		return "", fmt.Errorf("sign GitHub App JWT: %w", err)
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

type systemClock struct{}

func (systemClock) Now() time.Time {
	return time.Now()
}

func parseRSAPrivateKey(privateKeyPEM []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(privateKeyPEM)
	if block == nil {
		return nil, ErrInvalidPrivateKey
	}
	if privateKey, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		if err := privateKey.Validate(); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidPrivateKey, err)
		}
		return privateKey, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidPrivateKey, err)
	}
	privateKey, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, ErrInvalidPrivateKey
	}
	if err := privateKey.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidPrivateKey, err)
	}
	return privateKey, nil
}
