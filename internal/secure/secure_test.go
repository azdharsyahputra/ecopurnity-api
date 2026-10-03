package secure

import (
	"bytes"
	"testing"
)

func TestDeriveEncryptMAC(t *testing.T) {
	k, err := Derive([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(k.OTP, k.NIKHash) || bytes.Equal(k.NIKHash, k.NIKCipher) || len(k.NIKCipher) != 32 {
		t.Fatal("keys must be distinct 32-byte keys")
	}
	sealed, err := Encrypt(k.NIKCipher, []byte("3205010101900001"), []byte("user-1"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("3205010101900001")) {
		t.Fatal("plaintext visible")
	}
	if got, err := Decrypt(k.NIKCipher, sealed, []byte("user-1")); err != nil || string(got) != "3205010101900001" {
		t.Fatalf("decrypt: %q %v", got, err)
	}
	if _, err := Decrypt(k.NIKCipher, sealed, []byte("user-2")); err == nil {
		t.Fatal("ciphertext moved to another user must not decrypt")
	}
	if !bytes.Equal(MAC(k.NIKHash, "a", "b"), MAC(k.NIKHash, "a", "b")) || bytes.Equal(MAC(k.NIKHash, "ab", ""), MAC(k.NIKHash, "a", "b")) {
		t.Fatal("MAC must be deterministic and unambiguous")
	}
	if _, err := Derive([]byte("short")); err == nil {
		t.Fatal("short secret accepted")
	}
}
