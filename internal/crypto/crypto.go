// Package crypto provides optional AES-GCM sealing for OAuth tokens at rest.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
)

// Sealer encrypts and decrypts small blobs. A Sealer with a nil key is a passthrough.
type Sealer struct{ aead cipher.AEAD }

const plainPrefix = 0x00
const sealedPrefix = 0x01

func New(key []byte) (*Sealer, error) {
	if key == nil {
		return &Sealer{}, nil
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: aead}, nil
}

func (s *Sealer) Enabled() bool { return s.aead != nil }

// Seal returns a self-describing blob: a one-byte prefix marks it as plain or sealed.
func (s *Sealer) Seal(plain []byte) ([]byte, error) {
	if s.aead == nil {
		return append([]byte{plainPrefix}, plain...), nil
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := append([]byte{sealedPrefix}, nonce...)
	return s.aead.Seal(out, nonce, plain, nil), nil
}

func (s *Sealer) Open(blob []byte) ([]byte, error) {
	if len(blob) == 0 {
		return nil, errors.New("empty blob")
	}
	switch blob[0] {
	case plainPrefix:
		return blob[1:], nil
	case sealedPrefix:
		if s.aead == nil {
			return nil, errors.New("token is encrypted but db.encryption_key is not set")
		}
		ns := s.aead.NonceSize()
		if len(blob) < 1+ns {
			return nil, errors.New("sealed blob too short")
		}
		return s.aead.Open(nil, blob[1:1+ns], blob[1+ns:], nil)
	default:
		return nil, errors.New("unknown blob format")
	}
}
