package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"math/big"
)

// NewOTP returns a uniformly random 6-digit code (leading zeros kept).
func NewOTP() string {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("%06d", n.Int64())
}

// OTPHash is what the database stores for a code: HMAC-SHA256(secret, purpose|user|code). With only a million possible
// codes a plain hash would be reversible from a database dump; the key lives outside the database.
func OTPHash(secret []byte, purpose, userID, code string) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(purpose + "|" + userID + "|" + code))
	return m.Sum(nil)
}
