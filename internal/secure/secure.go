package secure

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
)

type Keys struct {
	OTP        []byte
	NIKHash    []byte
	NIKCipher  []byte
	BankCipher []byte
}

func Derive(secret []byte) (Keys, error) {
	if len(secret) < 32 {
		return Keys{}, errors.New("secret must be at least 32 bytes")
	}
	k := func(info string) ([]byte, error) { return hkdf.Key(sha256.New, secret, nil, info, 32) }
	var out Keys
	var err error
	if out.OTP, err = k("ecopurnity/otp/v1"); err != nil {
		return out, err
	}
	if out.NIKHash, err = k("ecopurnity/nik-hash/v1"); err != nil {
		return out, err
	}
	if out.NIKCipher, err = k("ecopurnity/nik-cipher/v1"); err != nil {
		return out, err
	}
	out.BankCipher, err = k("ecopurnity/bank-cipher/v1")
	return out, err
}

func MAC(key []byte, parts ...string) []byte {
	m := hmac.New(sha256.New, key)
	for i, p := range parts {
		if i > 0 {
			m.Write([]byte{0})
		}
		m.Write([]byte(p))
	}
	return m.Sum(nil)
}

func Encrypt(key, plaintext, aad []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, aad), nil
}

func Decrypt(key, sealed, aad []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(sealed) < gcm.NonceSize() {
		return nil, errors.New("ciphertext too short")
	}
	return gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], aad)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
