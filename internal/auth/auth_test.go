package auth

import (
	"bytes"
	"strings"
	"testing"
)

func TestPassword(t *testing.T) {
	h, err := HashPassword("rahasia-123")
	if err != nil || !strings.HasPrefix(h, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("hash %q %v", h, err)
	}
	if ok, _ := VerifyPassword("rahasia-123", h); !ok {
		t.Fatal("correct password rejected")
	}
	if ok, _ := VerifyPassword("rahasia-124", h); ok {
		t.Fatal("wrong password accepted")
	}
	if h2, _ := HashPassword("rahasia-123"); h2 == h {
		t.Fatal("salt not random")
	}
	if _, err := VerifyPassword("x", "$bcrypt$..."); err == nil {
		t.Fatal("malformed hash accepted")
	}
}

func TestToken(t *testing.T) {
	a, ha := NewToken()
	b, _ := NewToken()
	if a == b || len(a) != 43 || !bytes.Equal(ha, HashToken(a)) {
		t.Fatalf("tokens %q %q", a, b)
	}
}
