package auth

import (
	"strings"
	"testing"
	"time"
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

func TestSignerRoundTrip(t *testing.T) {
	signer, err := NewSigner()
	if err != nil {
		t.Fatal(err)
	}

	value := signer.Sign("admin", time.Minute)
	got, ok := signer.Verify(value)
	if !ok || got != "admin" {
		t.Fatalf("Verify(%q) = %q, %v", value, got, ok)
	}

	// A rewritten payload must not verify. Swapping one character of the
	// encoded payload keeps the shape and breaks the signature.
	parts := strings.SplitN(value, ".", 2)
	tampered := parts[0][:len(parts[0])-1] + "X" + "." + parts[1]
	if got, ok := signer.Verify(tampered); ok {
		t.Errorf("a rewritten payload verified as %q", got)
	}
	// Extending the expiry must not verify either.
	fields := strings.Split(value, ".")
	if _, ok := signer.Verify(fields[0] + ".9999999999." + fields[2]); ok {
		t.Error("a rewritten expiry verified")
	}
	for _, broken := range []string{"", "nonsense", "a.b", "a.b.c.d"} {
		if _, ok := signer.Verify(broken); ok {
			t.Errorf("Verify(%q) accepted a malformed value", broken)
		}
	}

	// An expired value must not verify.
	if _, ok := signer.Verify(signer.Sign("admin", -time.Second)); ok {
		t.Error("an expired value verified")
	}

	// Another signer has another secret.
	other, err := NewSigner()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := other.Verify(value); ok {
		t.Error("a value signed with a different secret verified")
	}
}

func TestSamePassword(t *testing.T) {
	if !SamePassword("filmkväll", "filmkväll") {
		t.Error("the right password was rejected")
	}
	if SamePassword("filmkvall", "filmkväll") {
		t.Error("a wrong password was accepted")
	}
	// A deployment with no password configured must not be open to anyone.
	if SamePassword("", "") || SamePassword("anything", "") {
		t.Error("an unset password matched")
	}
}
