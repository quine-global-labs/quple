package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"os"

	jose "github.com/go-jose/go-jose/v4"
)

// signingKey is the ES256 key every token is signed with. It's generated on
// first start and kept on disk, so tokens stay valid across restarts. It
// implements both op.SigningKey (private half) and op.Key (public half, for
// the JWKS).
type signingKey struct {
	id  string
	key *ecdsa.PrivateKey
}

func (k *signingKey) SignatureAlgorithm() jose.SignatureAlgorithm { return jose.ES256 }
func (k *signingKey) Algorithm() jose.SignatureAlgorithm          { return jose.ES256 }
func (k *signingKey) ID() string                                  { return k.id }
func (k *signingKey) Use() string                                 { return "sig" }

// Key returns the private key; publicKey below returns the public one. The
// library calls Key() on both interfaces, so they need distinct types.
func (k *signingKey) Key() any { return k.key }

type publicKey struct{ *signingKey }

func (k publicKey) Key() any { return &k.key.PublicKey }

// loadOrCreateSigningKey reads the PEM-encoded P-256 key at path, generating
// and saving one (mode 0600) if the file doesn't exist yet.
func loadOrCreateSigningKey(path string) (*signingKey, error) {
	raw, err := os.ReadFile(path)
	var key *ecdsa.PrivateKey
	switch {
	case err == nil:
		block, _ := pem.Decode(raw)
		if block == nil {
			return nil, fmt.Errorf("%s: not PEM", path)
		}
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		var ok bool
		if key, ok = parsed.(*ecdsa.PrivateKey); !ok || key.Curve != elliptic.P256() {
			return nil, fmt.Errorf("%s: want a P-256 ECDSA key", path)
		}
	case errors.Is(err, os.ErrNotExist):
		if key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader); err != nil {
			return nil, err
		}
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
			return nil, err
		}
	default:
		return nil, err
	}

	// Derive the key ID from the public key, so it changes iff the key does.
	pub, err := key.PublicKey.Bytes()
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(pub)
	return &signingKey{id: base64.RawURLEncoding.EncodeToString(sum[:12]), key: key}, nil
}
