package password

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/bcrypt"
)

const (
	memory  = 64 * 1024
	time    = 3
	threads = 4
	saltLen = 16
	keyLen  = 32
)

var ErrInvalidHash = errors.New("invalid password hash")

func Hash(pass []byte) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey(pass, salt, time, memory, threads, keyLen)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		memory, time, threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

func Verify(encoded string, pass []byte) (ok, needsRehash bool) {
	if strings.HasPrefix(encoded, "$2") {
		return bcrypt.CompareHashAndPassword([]byte(encoded), pass) == nil, true
	}
	var version, mem, iter int
	var lanes uint8
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" ||
		!scan(parts[2], "v=%d", &version) ||
		!scan(parts[3], "m=%d,t=%d,p=%d", &mem, &iter, &lanes) ||
		version != argon2.Version || mem < 8*1024 || mem > 64*1024 ||
		iter < 1 || iter > 10 || lanes < 1 || lanes > 4 {
		return false, false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < 8 || len(salt) > 64 {
		return false, false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) < 16 || len(want) > 64 {
		return false, false
	}
	got := argon2.IDKey(pass, salt, uint32(iter), uint32(mem), lanes, uint32(len(want)))
	ok = subtle.ConstantTimeCompare(got, want) == 1
	needsRehash = ok && (mem != memory || iter != time || lanes != threads ||
		len(salt) != saltLen || len(want) != keyLen)
	return ok, needsRehash
}

func scan(s, format string, args ...any) bool {
	n, err := fmt.Sscanf(s, format, args...)
	return err == nil && n == len(args)
}
