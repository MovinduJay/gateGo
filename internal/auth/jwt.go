package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Claims struct {
	Subject   string `json:"sub"`
	Role      string `json:"role"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
}
type Validator struct{ secret []byte }

func NewValidator(secret string) *Validator { return &Validator{secret: []byte(secret)} }
func (v *Validator) ValidateAuthorization(header string) (Claims, error) {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return Claims{}, errors.New("missing bearer token")
	}
	return v.Validate(parts[1])
}
func (v *Validator) Validate(raw string) (Claims, error) {
	if len(v.secret) == 0 {
		return Claims{}, errors.New("JWT secret not configured")
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return Claims{}, errors.New("malformed token")
	}
	mac := hmac.New(sha256.New, v.secret)
	_, _ = mac.Write([]byte(parts[0] + "." + parts[1]))
	expected := mac.Sum(nil)
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(sig, expected) {
		return Claims{}, errors.New("invalid signature")
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Claims{}, errors.New("bad header")
	}
	var h map[string]any
	if json.Unmarshal(headerBytes, &h) != nil || h["alg"] != "HS256" {
		return Claims{}, errors.New("unsupported algorithm")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, errors.New("bad payload")
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return Claims{}, err
	}
	if c.Subject == "" {
		return Claims{}, errors.New("subject required")
	}
	if c.ExpiresAt > 0 && time.Now().Unix() >= c.ExpiresAt {
		return Claims{}, errors.New("token expired")
	}
	return c, nil
}
func Issue(secret, subject, role string, ttl time.Duration) (string, error) {
	if secret == "" || subject == "" {
		return "", errors.New("secret and subject required")
	}
	now := time.Now()
	header, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	payload, _ := json.Marshal(Claims{Subject: subject, Role: role, IssuedAt: now.Unix(), ExpiresAt: now.Add(ttl).Unix()})
	h := base64.RawURLEncoding.EncodeToString(header)
	p := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = fmt.Fprint(mac, h+"."+p)
	return h + "." + p + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}
