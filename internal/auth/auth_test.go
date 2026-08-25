package auth

import (
	"strings"
	"testing"
)

func TestValidateUsername(t *testing.T) {
	valid := []string{"Ada", "bo_42", "Cleo Andersson", "Åsa-Britt", "x.y"}
	for _, name := range valid {
		if got, err := ValidateUsername(" " + name + " "); err != nil || got != name {
			t.Errorf("ValidateUsername(%q) = %q, %v; want %q with no error", name, got, err, name)
		}
	}

	invalid := []string{"", "a", "_leading", "double  space", strings.Repeat("x", 33), "semi;colon", "<script>"}
	for _, name := range invalid {
		if _, err := ValidateUsername(name); err == nil {
			t.Errorf("ValidateUsername(%q) should have been rejected", name)
		}
	}
}

func TestValidatePassword(t *testing.T) {
	if err := ValidatePassword("hunter2hunter2"); err != nil {
		t.Errorf("valid password rejected: %v", err)
	}
	for _, pw := range []string{"", "short", strings.Repeat("x", MaxPasswordLen+1)} {
		if err := ValidatePassword(pw); err == nil {
			t.Errorf("password of length %d should have been rejected", len(pw))
		}
	}
}

func TestHashAndCheckPassword(t *testing.T) {
	hash, err := HashPassword("filmkväll2026")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(hash, "filmkväll") {
		t.Error("hash must not contain the password")
	}
	if !CheckPassword(hash, "filmkväll2026") {
		t.Error("correct password rejected")
	}
	if CheckPassword(hash, "filmkväll2027") {
		t.Error("wrong password accepted")
	}
	if CheckPassword("not-a-hash", "filmkväll2026") {
		t.Error("garbage hash accepted")
	}
}

func TestTokensAreUniqueAndURLSafe(t *testing.T) {
	seen := make(map[string]bool, 64)
	for i := 0; i < 64; i++ {
		token, err := Token()
		if err != nil {
			t.Fatal(err)
		}
		if len(token) < 40 {
			t.Fatalf("token too short: %q", token)
		}
		if strings.ContainsAny(token, "+/=") {
			t.Errorf("token is not URL-safe: %q", token)
		}
		if seen[token] {
			t.Fatalf("duplicate token after %d draws", i)
		}
		seen[token] = true
	}
}
