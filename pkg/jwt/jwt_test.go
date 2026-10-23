package jwt

import (
	"bytes"
	"strings"
	"testing"
)

func TestParseUnverified_ValidToken(t *testing.T) {
	// Sample JWT:
	// Header: {"alg":"RS512","typ":"JWT","kid":"v1","jku":"https://product-tst.apis.f5networks.net/ee/v1/keys/jwks"}
	// Payload: {"sub":"TST-75550bac-03b5-4c29-9b8a-1a92a2b0a95e","iat":1776341100,"iss":"F5 Inc.","aud":"urn:f5:teem","jti":"7e704e50-398c-11f1-bc9c-e719460f5466","f5_order_type":"eval","f5_sat":1807833600}
	tokenStr := "eyJhbGciOiJSUzUxMiIsInR5cCI6IkpXVCIsImtpZCI6InYxIiwiamt1IjoiaHR0cHM6Ly9wcm9kdWN0LXRzdC5hcGlzLmY1bmV0d29ya3MubmV0L2VlL3YxL2tleXMvandrcyJ9.eyJzdWIiOiJUU1QtNzU1NTBiYWMtMDNiNS00YzI5LTliOGEtMWE5MmEyYjBhOTVlIiwiaWF0IjoxNzc2MzQxMTAwLCJpc3MiOiJGNSBJbmMuIiwiYXVkIjoidXJuOmY1OnRlZW0iLCJqdGkiOiI3ZTcwNGU1MC0zOThjLTExZjEtYmM5Yy1lNzE5NDYwZjU0NjYiLCJmNV9vcmRlcl90eXBlIjoiZXZhbCIsImY1X3NhdCI6MTgwNzgzMzYwMH0.signature-placeholder"

	tok, err := ParseUnverified(tokenStr)
	if err != nil {
		t.Fatalf("unexpected error parsing token: %v", err)
	}

	if tok.Header.Alg != "RS512" {
		t.Errorf("expected alg RS512, got %s", tok.Header.Alg)
	}
	if tok.Header.Kid != "v1" {
		t.Errorf("expected kid v1, got %s", tok.Header.Kid)
	}
	if tok.Header.Jku != "https://product-tst.apis.f5networks.net/ee/v1/keys/jwks" {
		t.Errorf("expected jku, got %s", tok.Header.Jku)
	}
	if tok.Claims.Sub != "TST-75550bac-03b5-4c29-9b8a-1a92a2b0a95e" {
		t.Errorf("expected sub, got %s", tok.Claims.Sub)
	}
	if tok.Claims.Iss != "F5 Inc." {
		t.Errorf("expected iss F5 Inc., got %s", tok.Claims.Iss)
	}
	if !tok.IsTestToken() {
		t.Errorf("expected IsTestToken to be true")
	}
}

func TestCheckPreflight_WarnsOnTestToken(t *testing.T) {
	tokenStr := "eyJhbGciOiJSUzUxMiIsInR5cCI6IkpXVCIsImtpZCI6InYxIiwiamt1IjoiaHR0cHM6Ly9wcm9kdWN0LXRzdC5hcGlzLmY1bmV0d29ya3MubmV0L2VlL3YxL2tleXMvandrcyJ9.eyJzdWIiOiJUU1QtMTIzNDUiLCJpc3MiOiJGNSBJbmMuIiwiZjVfc2F0IjoxODA3ODMzNjAwfQ.sig"

	var buf bytes.Buffer
	tok, err := CheckPreflight(tokenStr, &buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok == nil {
		t.Fatal("expected non-nil token")
	}
	output := buf.String()
	if !strings.Contains(output, "WARNING: TEST/EVALUATION JWT DETECTED WITH PRODUCTION ENDPOINTS") {
		t.Errorf("expected warning output, got: %s", output)
	}
	if !strings.Contains(output, "TST-12345") {
		t.Errorf("expected subject in warning output, got: %s", output)
	}
}

func TestCheckPreflight_NoWarnOnProdToken(t *testing.T) {
	// Prod token with product.apis.f5.com and non-TST subject
	// Header: {"alg":"RS512","typ":"JWT","kid":"v1","jku":"https://product.apis.f5.com/ee/v1/keys/jwks"}
	// Payload: {"sub":"PRD-99999","iss":"F5 Inc.","f5_sat":1807833600}
	hdr := "eyJhbGciOiJSUzUxMiIsInR5cCI6IkpXVCIsImtpZCI6InYxIiwiamt1IjoiaHR0cHM6Ly9wcm9kdWN0LmFwaXMuZjUuY29tL2VlL3YxL2tleXMvandrcyJ9"
	payload := "eyJzdWIiOiJQUkQtOTk5OTkiLCJpc3MiOiJGNSBJbmMuIiwiZjVfc2F0IjoxODA3ODMzNjAwfQ"
	tokenStr := hdr + "." + payload + ".sig"

	var buf bytes.Buffer
	tok, err := CheckPreflight(tokenStr, &buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok.IsTestToken() {
		t.Errorf("expected IsTestToken to be false for production token")
	}
	if buf.Len() > 0 {
		t.Errorf("expected no warning output for prod token, got: %s", buf.String())
	}
}

func TestCheckPreflight_Empty(t *testing.T) {
	var buf bytes.Buffer
	tok, err := CheckPreflight("", &buf)
	if err != nil {
		t.Fatalf("unexpected error for empty token: %v", err)
	}
	if tok != nil {
		t.Errorf("expected nil token for empty string")
	}
}

func TestParseUnverified_Invalid(t *testing.T) {
	_, err := ParseUnverified("invalid-token")
	if err == nil {
		t.Fatal("expected error for invalid token")
	}
}
