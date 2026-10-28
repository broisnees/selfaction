package github

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestAppSignsAndCachesInstallationTokens(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		claims := jwt.RegisteredClaims{}
		if _, err := jwt.ParseWithClaims(raw, &claims, func(*jwt.Token) (any, error) { return &key.PublicKey, nil },
			jwt.WithValidMethods([]string{"RS256"})); err != nil || claims.Issuer != "42" {
			http.Error(w, fmt.Sprintf("bad app jwt: %v %+v", err, claims), 401)
			return
		}
		if r.Method != "POST" || r.URL.Path != "/app/installations/7/access_tokens" {
			http.Error(w, "unexpected "+r.URL.Path, 404)
			return
		}
		calls.Add(1)
		fmt.Fprintf(w, `{"token":"ghs_%d","expires_at":%q}`, calls.Load(), time.Now().Add(time.Hour).Format(time.RFC3339))
	}))
	defer api.Close()

	pemFile := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(pemFile, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600); err != nil {
		t.Fatal(err)
	}
	app, err := LoadApp(42, pemFile, api.URL+"/")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		tok, err := app.Token(t.Context(), 7)
		if err != nil || tok != "ghs_1" {
			t.Fatalf("token = %q, %v", tok, err)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("%d token requests, want 1 (cached)", calls.Load())
	}
	if _, err := app.Token(t.Context(), 8); err == nil {
		t.Error("an unknown installation got a token")
	}
}
