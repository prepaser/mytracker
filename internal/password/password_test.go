package password_test

import (
	"strings"
	"testing"

	"github.com/prepaser/mytracker/internal/password"
	"golang.org/x/crypto/bcrypt"
)

func TestArgon2idRoundTrip(t *testing.T) {
	hash, err := password.Hash([]byte("correct horse battery staple"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=65536,t=3,p=4$") {
		t.Fatalf("hash=%q", hash)
	}
	ok, rehash := password.Verify(hash, []byte("correct horse battery staple"))
	if !ok || rehash {
		t.Fatalf("ok=%t rehash=%t", ok, rehash)
	}
	if ok, _ := password.Verify(hash, []byte("wrong password")); ok {
		t.Fatal("wrong password accepted")
	}
}

func TestBcryptRequiresRehash(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("legacy password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	ok, rehash := password.Verify(string(hash), []byte("legacy password"))
	if !ok || !rehash {
		t.Fatalf("ok=%t rehash=%t", ok, rehash)
	}
}

func TestMalformedHashIsRejected(t *testing.T) {
	for _, hash := range []string{
		"", "$argon2id$v=19$m=9999999,t=3,p=4$YWJj$YWJj",
		"$argon2id$v=19$m=65536,t=3,p=4$bad$bad",
	} {
		if ok, _ := password.Verify(hash, []byte("password")); ok {
			t.Fatalf("accepted %q", hash)
		}
	}
}
