package shop

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
)

type CardCipher struct {
	aead           cipher.AEAD
	fingerprintKey [32]byte
}

func NewCardCipher(key []byte) (*CardCipher, error) {
	if len(key) != 32 {
		return nil, errors.New("card key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	h := hmac.New(sha256.New, key)
	_, _ = h.Write([]byte("minimal-shop-card-fingerprint-v1"))
	var fingerprintKey [32]byte
	copy(fingerprintKey[:], h.Sum(nil))
	return &CardCipher{aead: aead, fingerprintKey: fingerprintKey}, nil
}

func (c *CardCipher) Fingerprint(plaintext string) []byte {
	h := hmac.New(sha256.New, c.fingerprintKey[:])
	_, _ = h.Write([]byte(plaintext))
	return h.Sum(nil)
}

func (c *CardCipher) Encrypt(productID int64, plaintext string) ([]byte, []byte, error) {
	if plaintext == "" {
		return nil, nil, errors.New("empty card")
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, err
	}
	data := c.aead.Seal(nil, nonce, []byte(plaintext), cardAAD(productID))
	return nonce, data, nil
}

func (c *CardCipher) Decrypt(productID int64, nonce, ciphertext []byte) (string, error) {
	plaintext, err := c.aead.Open(nil, nonce, ciphertext, cardAAD(productID))
	if err != nil {
		return "", errors.New("card decrypt failed")
	}
	return string(plaintext), nil
}

func cardAAD(productID int64) []byte {
	var b [8]byte
	for i := 7; i >= 0; i-- {
		b[i] = byte(productID)
		productID >>= 8
	}
	return b[:]
}

func randomToken() (string, [32]byte, error) {
	var raw [32]byte
	if _, err := io.ReadFull(rand.Reader, raw[:]); err != nil {
		return "", [32]byte{}, err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), sha256.Sum256(raw[:]), nil
}

func tokenHash(encoded string) ([32]byte, error) {
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(raw) != 32 {
		return [32]byte{}, errors.New("invalid session token")
	}
	return sha256.Sum256(raw), nil
}
