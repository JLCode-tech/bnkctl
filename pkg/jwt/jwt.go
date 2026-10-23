// Package jwt provides unverified inspection and preflight checks for F5 BNK
// subscription JWT tokens.
package jwt

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// Header represents the unverified header of an F5 subscription JWT.
type Header struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	Jku string `json:"jku"`
	Typ string `json:"typ,omitempty"`
}

// Claims represents unverified payload claims of an F5 subscription JWT.
type Claims struct {
	Sub         string `json:"sub"`
	Iss         string `json:"iss"`
	F5Sat       any    `json:"f5_sat"`
	Aud         any    `json:"aud,omitempty"`
	Iat         int64  `json:"iat,omitempty"`
	Exp         int64  `json:"exp,omitempty"`
	F5OrderType string `json:"f5_order_type,omitempty"`
}

// Token represents a parsed, unverified JWT.
type Token struct {
	Raw    string
	Header Header
	Claims Claims
}

// TokenInfo summarizes key unverified JWT metadata for CLI display and preflight checks.
type TokenInfo struct {
	KeyID     string
	JKU       string
	Subject   string
	Issuer    string
	ExpiresAt time.Time
	IsTestEnv bool
	IsExpired bool
}

// ParseUnverified decodes the header and payload of a 3-part JWT token without signature verification.
func ParseUnverified(tokenStr string) (*Token, error) {
	tokenStr = strings.TrimSpace(tokenStr)
	parts := strings.Split(tokenStr, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid token: expected 3 dot-separated segments, got %d", len(parts))
	}

	hdrBytes, err := decodeSegment(parts[0])
	if err != nil {
		return nil, fmt.Errorf("decoding token header: %w", err)
	}
	var hdr Header
	if err := json.Unmarshal(hdrBytes, &hdr); err != nil {
		return nil, fmt.Errorf("unmarshaling token header: %w", err)
	}

	payloadBytes, err := decodeSegment(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decoding token payload: %w", err)
	}
	var claims Claims
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return nil, fmt.Errorf("unmarshaling token payload: %w", err)
	}

	return &Token{
		Raw:    tokenStr,
		Header: hdr,
		Claims: claims,
	}, nil
}

func decodeSegment(seg string) ([]byte, error) {
	if b, err := base64.RawURLEncoding.DecodeString(seg); err == nil {
		return b, nil
	}
	if b, err := base64.URLEncoding.DecodeString(seg); err == nil {
		return b, nil
	}
	pad := len(seg) % 4
	if pad > 0 {
		seg += strings.Repeat("=", 4-pad)
	}
	return base64.URLEncoding.DecodeString(seg)
}

// IsTestToken reports whether the JWT points to test infrastructure (product-tst or TST- subject prefix).
func (t *Token) IsTestToken() bool {
	if strings.Contains(strings.ToLower(t.Header.Jku), "product-tst") {
		return true
	}
	if strings.HasPrefix(t.Claims.Sub, "TST-") {
		return true
	}
	return false
}

// Inspect returns a TokenInfo summary for a raw JWT string.
func Inspect(rawJWT string) (*TokenInfo, error) {
	tok, err := ParseUnverified(rawJWT)
	if err != nil {
		return nil, err
	}
	info := &TokenInfo{
		KeyID:     tok.Header.Kid,
		JKU:       tok.Header.Jku,
		Subject:   tok.Claims.Sub,
		Issuer:    tok.Claims.Iss,
		IsTestEnv: tok.IsTestToken(),
	}
	if tok.Claims.Exp > 0 {
		info.ExpiresAt = time.Unix(tok.Claims.Exp, 0).UTC()
		info.IsExpired = time.Now().UTC().After(info.ExpiresAt)
	}
	return info, nil
}

// CheckPreflight inspects a JWT string and emits a warning to out (defaults to os.Stderr)
// if a test token is detected while production endpoints are configured.
func CheckPreflight(jwtStr string, out io.Writer) (*Token, error) {
	if out == nil {
		out = os.Stderr
	}
	jwtStr = strings.TrimSpace(jwtStr)
	if jwtStr == "" {
		return nil, nil
	}
	tok, err := ParseUnverified(jwtStr)
	if err != nil {
		return nil, err
	}

	if tok.IsTestToken() {
		fmt.Fprintln(out, "================================================================================")
		fmt.Fprintln(out, "WARNING: TEST/EVALUATION JWT DETECTED WITH PRODUCTION ENDPOINTS")
		fmt.Fprintf(out, "  Subject: %s\n", tok.Claims.Sub)
		fmt.Fprintf(out, "  Issuer:  %s\n", tok.Claims.Iss)
		fmt.Fprintf(out, "  JKU:     %s\n", tok.Header.Jku)
		fmt.Fprintln(out, "  The JWT appears to be a test/evaluation token (product-tst or TST- subject),")
		fmt.Fprintln(out, "  but default production endpoints (https://product.apis.f5.com) are configured.")
		fmt.Fprintln(out, "  This may cause licensing validation failure or deadlocks.")
		fmt.Fprintln(out, "================================================================================")
	}
	return tok, nil
}
