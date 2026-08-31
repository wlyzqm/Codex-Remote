package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	minPasswordBytes = 12
	maxPasswordBytes = 1024
	maxConfigBytes   = 64 << 10
	sessionKeyBytes  = 32
)

type Config struct {
	Password string `json:"password"`
}

// LoadConfig reads the operator-managed password from a private JSON file.
// The file is intentionally not created by the receiver: startup must fail
// until the operator has explicitly chosen a password.
func LoadConfig(path string) (Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return Config{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return Config{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return Config{}, errors.New("config file must be a private regular file (mode 0600 or stricter)")
	}
	decoder := json.NewDecoder(io.LimitReader(file, maxConfigBytes+1))
	decoder.DisallowUnknownFields()
	var config Config
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if config.Password != strings.TrimSpace(config.Password) {
		return Config{}, errors.New("password must not start or end with whitespace")
	}
	if len(config.Password) < minPasswordBytes {
		return Config{}, fmt.Errorf("password must contain at least %d bytes", minPasswordBytes)
	}
	if len(config.Password) > maxPasswordBytes {
		return Config{}, errors.New("password is too long")
	}
	return config, nil
}

func EqualPassword(expected, supplied string) bool {
	want := sha256.Sum256([]byte("codex-remote/password/v1\x00" + expected))
	got := sha256.Sum256([]byte("codex-remote/password/v1\x00" + supplied))
	return subtle.ConstantTimeCompare(want[:], got[:]) == 1
}

func NewSessionKey() (string, error) {
	data := make([]byte, sessionKeyBytes)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

// IssueSession returns a stateless cookie signed by the receiver's ephemeral
// session key. Restarting the receiver invalidates every existing session.
func IssueSession(sessionKey string, expiry time.Time) (string, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	expires := strconv.FormatInt(expiry.Unix(), 10)
	nonceText := base64.RawURLEncoding.EncodeToString(nonce)
	payload := "v1." + expires + "." + nonceText
	signature := sign(sessionKey, payload)
	return payload + "." + signature, nil
}

func ValidateSession(sessionKey, value string, now time.Time) bool {
	parts := strings.Split(value, ".")
	if len(parts) != 4 || parts[0] != "v1" {
		return false
	}
	expires, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || now.Unix() >= expires {
		return false
	}
	payload := strings.Join(parts[:3], ".")
	want := sign(sessionKey, payload)
	if len(want) != len(parts[3]) {
		return false
	}
	return hmac.Equal([]byte(want), []byte(parts[3]))
}

func sign(sessionKey, payload string) string {
	key := sha256.Sum256([]byte("codex-remote/session/v1\x00" + sessionKey))
	mac := hmac.New(sha256.New, key[:])
	_, _ = mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
