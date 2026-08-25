// Package auth handles password hashing, token generation and the small amount
// of input validation that guards the sign-up form.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
)

const (
	MinPasswordLen = 8
	MaxPasswordLen = 128 // bcrypt silently truncates past 72 bytes; reject early
)

// Usernames stay friendly but printable: letters (including åäö), digits and
// a few separators.
var usernameRe = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N}_.\- ]{1,31}$`)

var (
	ErrBadUsername = errors.New("username must be 2-32 characters: letters, digits, space, - _ .")
	ErrBadPassword = fmt.Errorf("password must be %d-%d characters", MinPasswordLen, MaxPasswordLen)
)

func ValidateUsername(name string) (string, error) {
	name = strings.TrimSpace(name)
	if !usernameRe.MatchString(name) || strings.Contains(name, "  ") {
		return "", ErrBadUsername
	}
	return name, nil
}

func ValidatePassword(pw string) error {
	if n := utf8.RuneCountInString(pw); n < MinPasswordLen || n > MaxPasswordLen {
		return ErrBadPassword
	}
	return nil
}

func HashPassword(pw string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	return string(h), nil
}

// CheckPassword reports whether pw matches hash. It is deliberately quiet
// about which half was wrong.
func CheckPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// Token returns a URL-safe 256-bit random string, used for both session
// cookies and CSRF tokens.
func Token() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

/* ------------------------------------------------- short-lived signed values --- */

// Signer signs values that live in a cookie for a few minutes. It carries the
// half-finished login in Mattermost mode: the password has been accepted, but
// the person has not said who they are yet, so there is no account to hang a
// session on.
//
// The secret is generated at startup, so a restart drops any half-finished
// login. That is the right trade: they are minutes old and one click from being
// made again.
type Signer struct {
	secret []byte
}

// NewSigner builds a signer with a fresh random secret.
func NewSigner() (*Signer, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("read random: %w", err)
	}
	return &Signer{secret: secret}, nil
}

// Sign returns payload with an expiry and a signature over both.
func (s *Signer) Sign(payload string, ttl time.Duration) string {
	body := base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		strconv.FormatInt(time.Now().Add(ttl).Unix(), 10)
	return body + "." + s.mac(body)
}

// Verify checks a value from Sign and returns the payload. A tampered or
// expired value returns false.
func (s *Signer) Verify(value string) (string, bool) {
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return "", false
	}
	body := parts[0] + "." + parts[1]
	if subtle.ConstantTimeCompare([]byte(s.mac(body)), []byte(parts[2])) != 1 {
		return "", false
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || time.Now().After(time.Unix(exp, 0)) {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", false
	}
	return string(payload), true
}

func (s *Signer) mac(body string) string {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(body))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// SamePassword compares a submitted password with the configured one in
// constant time. An empty configured password never matches, so a half-set
// deployment cannot be walked into with a blank field.
func SamePassword(submitted, configured string) bool {
	if configured == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(submitted), []byte(configured)) == 1
}
