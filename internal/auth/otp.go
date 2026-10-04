package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"math/big"
)

func NewOTP() string {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("%06d", n.Int64())
}

func OTPHash(secret []byte, purpose, userID, code string) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(purpose + "|" + userID + "|" + code))
	return m.Sum(nil)
}
