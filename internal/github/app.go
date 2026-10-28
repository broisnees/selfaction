package github

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// App authenticates as a GitHub App: it signs a short-lived JWT with the
// app's private key and exchanges it for installation tokens, which are
// scoped to the repositories the app is installed on and expire after an hour.
type App struct {
	ID   int64
	Key  *rsa.PrivateKey
	API  string // https://api.github.com, or https://<host>/api/v3
	HTTP *http.Client

	mu     sync.Mutex
	tokens map[int64]installationToken
}

type installationToken struct {
	token   string
	expires time.Time
}

// LoadApp reads the app's private key, the .pem file GitHub lets you download.
func LoadApp(id int64, keyFile, api string) (*App, error) {
	b, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, err
	}
	key, err := ParsePrivateKey(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", keyFile, err)
	}
	return &App{ID: id, Key: key, API: strings.TrimSuffix(api, "/")}, nil
}

// ParsePrivateKey reads a PEM RSA key in PKCS#1 (what GitHub issues) or PKCS#8 form.
func ParsePrivateKey(b []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("no PEM data")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("not an RSA key")
	}
	return rk, nil
}

// JWT is the app's own credential, valid for ten minutes. GitHub allows for
// clock drift by accepting a token issued up to a minute in the past.
func (a *App) JWT() (string, error) {
	now := time.Now()
	return jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{
		Issuer:    fmt.Sprint(a.ID),
		IssuedAt:  jwt.NewNumericDate(now.Add(-60 * time.Second)),
		ExpiresAt: jwt.NewNumericDate(now.Add(9 * time.Minute)),
	}).SignedString(a.Key)
}

// Token returns a token for an installation, reusing one that is valid for at
// least ten more minutes.
func (a *App) Token(ctx context.Context, installation int64) (string, error) {
	a.mu.Lock()
	if t, ok := a.tokens[installation]; ok && time.Until(t.expires) > 10*time.Minute {
		a.mu.Unlock()
		return t.token, nil
	}
	a.mu.Unlock()

	signed, err := a.JWT()
	if err != nil {
		return "", err
	}
	c := &Client{API: a.API, Token: signed, HTTP: a.HTTP}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := c.Do(ctx, "POST", fmt.Sprintf("/app/installations/%d/access_tokens", installation), nil, &out); err != nil {
		return "", fmt.Errorf("installation token for %d: %w", installation, err)
	}
	a.mu.Lock()
	if a.tokens == nil {
		a.tokens = map[int64]installationToken{}
	}
	a.tokens[installation] = installationToken{out.Token, out.ExpiresAt}
	a.mu.Unlock()
	return out.Token, nil
}
