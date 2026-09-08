package auth

import (
	"strings"
	"testing"
	"time"
)

func TestIssueAndValidate(t *testing.T) {
	secret := "test-secret"
	token, err := Issue(secret, "alice", "admin", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := NewValidator(secret).Validate(token)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Subject != "alice" || claims.Role != "admin" {
		t.Fatalf("unexpected claims: %+v", claims)
	}
}

func TestRejectTamperedToken(t *testing.T) {
	token, _ := Issue("secret", "alice", "user", time.Minute)
	parts := strings.Split(token, ".")
	parts[1] = parts[1] + "x"
	if _, err := NewValidator("secret").Validate(strings.Join(parts, ".")); err == nil {
		t.Fatal("expected validation failure")
	}
}
