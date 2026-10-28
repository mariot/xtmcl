package platform

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
)

// encryptionVersion 2 is RSA-OAEP (SHA-256, MGF1 SHA-256) + AES-256-GCM, the scheme
// the platform uses for encrypted configuration values (version 1, PKCS#1 v1.5,
// is refused by FIPS builds of the Composer).
const encryptionVersion = 2

// GenerateKey creates the 4096-bit RSA key the Composer uses as manager.credentials_key.
func GenerateKey(path string) error {
	key, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		return fmt.Errorf("generate RSA key: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return fmt.Errorf("encode RSA key: %w", err)
	}
	data := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	return os.WriteFile(path, data, 0o600)
}

// LoadPublicKey reads the public half of the PEM private key at path.
func LoadPublicKey(path string) (*rsa.PublicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("no PEM block in " + path)
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("not an RSA private key: " + path)
	}
	return &rsaKey.PublicKey, nil
}

// ParsePublicKeyPEM parses the SPKI PEM public key the Composer sends when it registers.
func ParsePublicKeyPEM(data string) (*rsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(data))
	if block == nil {
		return nil, errors.New("no PEM block in public key")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse public key: %w", err)
	}
	rsaKey, ok := key.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("not an RSA public key")
	}
	return rsaKey, nil
}

// Encrypt encrypts value for the Composer holding the private key of pub:
// base64(version byte || RSA-OAEP(AES key || IV) || AES-GCM(value)).
func Encrypt(pub *rsa.PublicKey, value string) (string, error) {
	keyIV := make([]byte, 32+12)
	if _, err := rand.Read(keyIV); err != nil {
		return "", err
	}
	wrapped, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, pub, keyIV, nil)
	if err != nil {
		return "", fmt.Errorf("wrap AES key: %w", err)
	}
	block, err := aes.NewCipher(keyIV[:32])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	sealed := gcm.Seal(nil, keyIV[32:], []byte(value), nil)

	out := make([]byte, 0, 1+len(wrapped)+len(sealed))
	out = append(out, encryptionVersion)
	out = append(out, wrapped...)
	out = append(out, sealed...)
	return base64.StdEncoding.EncodeToString(out), nil
}
