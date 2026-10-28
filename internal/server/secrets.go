package server

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Sealer encrypts repository secrets at rest with AES-256-GCM. Each value is
// bound to its repository and name, so a sealed value copied to another row
// does not decrypt.
type Sealer struct{ aead cipher.AEAD }

// LoadSealer reads a 32-byte hex key from path, creating it (mode 0600) on first use.
func LoadSealer(path string) (*Sealer, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		b = []byte(hex.EncodeToString(key))
		if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("%s: want 64 hex characters", path)
	}
	return NewSealer(key)
}

func NewSealer(key []byte) (*Sealer, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: aead}, nil
}

func secretAAD(repo, name string) []byte { return []byte(strings.ToLower(repo) + "\x00" + name) }

func (s *Sealer) Seal(repo, name string, value []byte) []byte {
	nonce := make([]byte, s.aead.NonceSize())
	_, _ = rand.Read(nonce)
	return s.aead.Seal(nonce, nonce, value, secretAAD(repo, name))
}

func (s *Sealer) Open(repo, name string, sealed []byte) ([]byte, error) {
	n := s.aead.NonceSize()
	if len(sealed) < n {
		return nil, errors.New("sealed value too short")
	}
	return s.aead.Open(nil, sealed[:n], sealed[n:], secretAAD(repo, name))
}

// Secret names follow GitHub's rules, and like GitHub's they are case-insensitive.
var secretName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

func normalizeSecretName(name string) (string, error) {
	name = strings.ToUpper(name)
	switch {
	case !secretName.MatchString(name):
		return "", errors.New("secret names may only contain letters, digits and _, and may not start with a digit")
	case strings.HasPrefix(name, "GITHUB_"):
		return "", errors.New("secret names may not start with GITHUB_")
	case len(name) > 200:
		return "", errors.New("secret name too long")
	}
	return name, nil
}

// SecretInfo describes a stored secret without its value.
type SecretInfo struct {
	Name      string    `json:"name"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (s *Store) SetSecret(ctx context.Context, repo, name string, sealed []byte) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO secrets (repo, name, value, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (repo, name) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		strings.ToLower(repo), name, sealed, time.Now().Unix())
	return err
}

func (s *Store) DeleteSecret(ctx context.Context, repo, name string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM secrets WHERE repo = ? AND name = ?`, strings.ToLower(repo), name)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// SealedSecrets returns a repository's sealed secrets by name.
func (s *Store) SealedSecrets(ctx context.Context, repo string) (map[string][]byte, []SecretInfo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, value, updated_at FROM secrets WHERE repo = ? ORDER BY name`, strings.ToLower(repo))
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	values := map[string][]byte{}
	var infos []SecretInfo
	for rows.Next() {
		var name string
		var value []byte
		var at int64
		if err := rows.Scan(&name, &value, &at); err != nil {
			return nil, nil, err
		}
		values[name] = value
		infos = append(infos, SecretInfo{Name: name, UpdatedAt: time.Unix(at, 0)})
	}
	return values, infos, rows.Err()
}

// repoSecrets decrypts a repository's secrets for a job.
func (s *Server) repoSecrets(ctx context.Context, repo string) (map[string]string, error) {
	sealed, _, err := s.Store.SealedSecrets(ctx, repo)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(sealed))
	for name, v := range sealed {
		plain, err := s.Sealer.Open(repo, name, v)
		if err != nil {
			return nil, fmt.Errorf("secret %s: %w (was the key in <home>/server/secrets.key replaced?)", name, err)
		}
		out[name] = string(plain)
	}
	return out, nil
}

func (s *Server) secretsEnabled(w http.ResponseWriter) bool {
	if s.Sealer == nil {
		http.Error(w, "secrets are turned off on this server", http.StatusNotImplemented)
		return false
	}
	return true
}

func (s *Server) listSecrets(w http.ResponseWriter, r *http.Request) {
	if !s.secretsEnabled(w) {
		return
	}
	_, infos, err := s.Store.SealedSecrets(r.Context(), r.PathValue("owner")+"/"+r.PathValue("repo"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if infos == nil {
		infos = []SecretInfo{}
	}
	writeJSON(w, infos)
}

// putSecret stores the request body as the secret's value.
func (s *Server) putSecret(w http.ResponseWriter, r *http.Request) {
	if !s.secretsEnabled(w) {
		return
	}
	name, err := normalizeSecretName(r.PathValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	value, err := io.ReadAll(io.LimitReader(r.Body, 48<<10+1))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(value) > 48<<10 {
		http.Error(w, "secrets are limited to 48 KB, as on GitHub", http.StatusRequestEntityTooLarge)
		return
	}
	repo := r.PathValue("owner") + "/" + r.PathValue("repo")
	if err := s.Store.SetSecret(r.Context(), repo, name, s.Sealer.Seal(repo, name, value)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteSecret(w http.ResponseWriter, r *http.Request) {
	if !s.secretsEnabled(w) {
		return
	}
	name, err := normalizeSecretName(r.PathValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ok, err := s.Store.DeleteSecret(r.Context(), r.PathValue("owner")+"/"+r.PathValue("repo"), name)
	switch {
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	case !ok:
		http.NotFound(w, r)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
