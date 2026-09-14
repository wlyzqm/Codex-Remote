package auth

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"time"
)

// Account cookies bind the account revision to the existing signed session.
func IssueAccountSession(key, account string, expiry time.Time) (string, error) {
	token, err := IssueSession(key+"\x00"+account, expiry)
	return base64.RawURLEncoding.EncodeToString([]byte(account)) + "~" + token, err
}
func SessionAccount(key, token string, now time.Time) (string, bool) {
	encoded, session, ok := strings.Cut(token, "~")
	if !ok {
		return "", ValidateSession(key, token, now)
	}
	account, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(account) > 200 || len(account) == 0 {
		return "", false
	}
	return string(account), ValidateSession(key+"\x00"+string(account), session, now)
}
func HashPassword(password string) (string, error) {
	if len(password) < 12 || len(password) > 1024 || strings.TrimSpace(password) != password {
		return "", errors.New("密码需为 12 至 1024 字节，首尾不能有空白")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hash, err := pbkdf2.Key(sha256.New, password, salt, 600000, 32)
	return base64.RawStdEncoding.EncodeToString(salt) + "." + base64.RawStdEncoding.EncodeToString(hash), err
}
func CheckPassword(hash, password string) bool {
	if len(password) > 1024 {
		return false
	}
	parts := strings.Split(hash, ".")
	if len(parts) != 2 {
		return false
	}
	salt, e1 := base64.RawStdEncoding.DecodeString(parts[0])
	want, e2 := base64.RawStdEncoding.DecodeString(parts[1])
	if e1 != nil || e2 != nil || len(salt) != 16 || len(want) != 32 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, 600000, 32)
	return err == nil && hmac.Equal(got, want)
}
