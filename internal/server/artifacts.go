package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/nektos/act/pkg/artifacts"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// The artifact service speaks the protocol of actions/upload-artifact and
// download-artifact v4: Twirp calls under artifactService, authenticated
// with the ACTIONS_RUNTIME_TOKEN a job got with its claim, and blob uploads
// and downloads through signed URLs. Artifacts belong to a run, so a job on
// one worker downloads what a job on another uploaded.

const artifactService = "/twirp/github.actions.results.api.v1.ArtifactService"

// Artifact is a file a job uploaded.
type Artifact struct {
	ID        int64     `json:"id"`
	RunID     string    `json:"run_id"`
	JobID     string    `json:"job_id"`
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	Digest    string    `json:"digest,omitempty"` // sha256:<hex> of the stored file
	Mime      string    `json:"mime,omitempty"`   // "" or application/zip for a zipped artifact
	Ready     bool      `json:"-"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

const artifactCols = `id, run_id, job_id, name, size, digest, mime, ready, created_at, expires_at`

func scanArtifact(sc scanner) (*Artifact, error) {
	var a Artifact
	var created, expires int64
	if err := sc.Scan(&a.ID, &a.RunID, &a.JobID, &a.Name, &a.Size, &a.Digest, &a.Mime, &a.Ready, &created, &expires); err != nil {
		return nil, err
	}
	a.CreatedAt, a.ExpiresAt = time.Unix(created, 0), time.Unix(expires, 0)
	return &a, nil
}

func (s *Store) queryArtifacts(ctx context.Context, where string, args ...any) ([]*Artifact, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+artifactCols+` FROM artifacts `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Artifact
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Artifacts lists a run's finished artifacts by name.
func (s *Store) Artifacts(ctx context.Context, runID string) ([]*Artifact, error) {
	return s.queryArtifacts(ctx, `WHERE run_id = ? AND ready = 1 ORDER BY name`, runID)
}

func (s *Store) artifact(ctx context.Context, where string, args ...any) (*Artifact, error) {
	as, err := s.queryArtifacts(ctx, where+` LIMIT 1`, args...)
	if err != nil {
		return nil, err
	}
	if len(as) == 0 {
		return nil, ErrNotFound
	}
	return as[0], nil
}

func (s *Server) artifactPath(id int64) string {
	return filepath.Join(s.Artifacts, strconv.FormatInt(id, 10)+".zip")
}

func (s *Server) blockDir(id int64) string {
	return filepath.Join(s.Artifacts, strconv.FormatInt(id, 10)+".blocks")
}

// removeArtifact deletes an artifact's record and files.
func (s *Server) removeArtifact(ctx context.Context, id int64) error {
	_ = os.RemoveAll(s.blockDir(id))
	if err := os.Remove(s.artifactPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_, err := s.Store.db.ExecContext(ctx, `DELETE FROM artifacts WHERE id = ?`, id)
	return err
}

// expireArtifacts removes artifacts past their retention, and uploads that
// never finished.
func (s *Server) expireArtifacts(ctx context.Context) (int, error) {
	now := time.Now()
	old, err := s.Store.queryArtifacts(ctx, `WHERE expires_at < ? OR (ready = 0 AND created_at < ?)`, now.Unix(), now.Add(-24*time.Hour).Unix())
	if err != nil {
		return 0, err
	}
	for _, a := range old {
		if err := s.removeArtifact(ctx, a.ID); err != nil {
			return 0, err
		}
	}
	return len(old), nil
}

// runtimeToken is a job's ACTIONS_RUNTIME_TOKEN. @actions/artifact reads the
// run and job ids from its scp claim; the server checks its signature.
func (s *Server) runtimeToken(runID, jobID string) string {
	now := time.Now()
	t, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"scp": "Actions.Results:" + runID + ":" + jobID,
		"iat": now.Unix(),
		"nbf": now.Add(-time.Minute).Unix(),
		"exp": now.Add(24 * time.Hour).Unix(),
	}).SignedString(s.signingKey())
	return t
}

// tokenJob checks a runtime token and returns the running job it was issued to.
func (s *Server) tokenJob(r *http.Request) (*Job, error) {
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return nil, errors.New("missing ACTIONS_RUNTIME_TOKEN")
	}
	claims := jwt.MapClaims{}
	if _, err := jwt.ParseWithClaims(raw, claims, func(*jwt.Token) (any, error) { return s.signingKey(), nil },
		jwt.WithValidMethods([]string{"HS256"})); err != nil {
		return nil, err
	}
	scp, _ := claims["scp"].(string)
	parts := strings.Split(scp, ":")
	if len(parts) != 3 || parts[0] != "Actions.Results" {
		return nil, errors.New("token has no Actions.Results scope")
	}
	j, err := s.Store.Get(r.Context(), parts[2])
	if err != nil {
		return nil, err
	}
	if j.RunID != parts[1] || j.Status != Running {
		return nil, errors.New("the job this token belongs to is not running")
	}
	return j, nil
}

// twirpError answers a Twirp call with an error.
func twirpError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"code":%q,"msg":%q}`, code, msg)
}

func validArtifactName(name string) error {
	if name == "" || len(name) > 256 || strings.ContainsAny(name, "\"/\\:<>|*?\r\n\x00") || name == "." || name == ".." {
		return fmt.Errorf("invalid artifact name %q", name)
	}
	return nil
}

// sign makes a URL for one artifact's blob that works until exp.
func (s *Server) blobURL(id int64, op string, exp time.Time) string {
	e := strconv.FormatInt(exp.Unix(), 10)
	return fmt.Sprintf("%s/artifacts/blob/%d?op=%s&se=%s&sig=%s", s.base(), id, op, e, s.blobSig(id, op, e))
}

func (s *Server) blobSig(id int64, op, exp string) string {
	mac := hmac.New(sha256.New, s.signingKey())
	fmt.Fprintf(mac, "%d\x00%s\x00%s", id, op, exp)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// blobArtifact checks a signed blob URL and returns its artifact.
func (s *Server) blobArtifact(r *http.Request, op string) (*Artifact, error) {
	q := r.URL.Query()
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || q.Get("op") != op {
		return nil, errors.New("invalid link")
	}
	exp, err := strconv.ParseInt(q.Get("se"), 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return nil, errors.New("link expired")
	}
	if !hmac.Equal([]byte(q.Get("sig")), []byte(s.blobSig(id, op, q.Get("se")))) {
		return nil, errors.New("invalid signature")
	}
	return s.Store.artifact(r.Context(), `WHERE id = ?`, id)
}

type rpc interface {
	proto.Message
	GetWorkflowRunBackendId() string
}

// artifactRPC serves the Twirp ArtifactService.
func (s *Server) artifactRPC(w http.ResponseWriter, r *http.Request) {
	job, err := s.tokenJob(r)
	if err != nil {
		twirpError(w, http.StatusUnauthorized, "unauthenticated", err.Error())
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		twirpError(w, http.StatusBadRequest, "malformed", err.Error())
		return
	}
	read := func(m rpc) bool {
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(body, m); err != nil {
			twirpError(w, http.StatusBadRequest, "malformed", err.Error())
			return false
		}
		if m.GetWorkflowRunBackendId() != job.RunID {
			twirpError(w, http.StatusForbidden, "permission_denied", "the token belongs to another run")
			return false
		}
		return true
	}
	reply := func(m proto.Message) {
		b, err := protojson.Marshal(m)
		if err != nil {
			twirpError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
	}
	fail := func(err error) {
		if errors.Is(err, ErrNotFound) {
			twirpError(w, http.StatusNotFound, "not_found", "artifact not found")
			return
		}
		twirpError(w, http.StatusInternalServerError, "internal", err.Error())
	}
	ctx := r.Context()

	switch r.PathValue("method") {
	case "CreateArtifact":
		var req artifacts.CreateArtifactRequest
		if !read(&req) {
			return
		}
		if err := validArtifactName(req.Name); err != nil {
			twirpError(w, http.StatusBadRequest, "invalid_argument", err.Error())
			return
		}
		if _, err := s.Store.artifact(ctx, `WHERE run_id = ? AND name = ? AND ready = 1`, job.RunID, req.Name); err == nil {
			twirpError(w, http.StatusConflict, "already_exists", "an artifact with this name already exists on the workflow run")
			return
		}
		stale, _ := s.Store.queryArtifacts(ctx, `WHERE run_id = ? AND name = ? AND ready = 0`, job.RunID, req.Name)
		for _, a := range stale {
			_ = s.removeArtifact(ctx, a.ID)
		}
		retention := s.ArtifactRetention
		if retention <= 0 {
			retention = 90 * 24 * time.Hour
		}
		expires := time.Now().Add(retention)
		if t := req.GetExpiresAt(); t != nil && t.AsTime().Before(expires) && t.AsTime().After(time.Now()) {
			expires = t.AsTime()
		}
		res, err := s.Store.db.ExecContext(ctx, `INSERT INTO artifacts (run_id, job_id, name, mime, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?)`,
			job.RunID, job.ID, req.Name, mimeType(body), time.Now().Unix(), expires.Unix())
		if err != nil {
			fail(err)
			return
		}
		id, _ := res.LastInsertId()
		if err := os.MkdirAll(s.Artifacts, 0o755); err != nil {
			fail(err)
			return
		}
		reply(&artifacts.CreateArtifactResponse{Ok: true, SignedUploadUrl: s.blobURL(id, "u", time.Now().Add(6*time.Hour))})

	case "FinalizeArtifact":
		var req artifacts.FinalizeArtifactRequest
		if !read(&req) {
			return
		}
		a, err := s.Store.artifact(ctx, `WHERE run_id = ? AND name = ? AND ready = 0 ORDER BY id DESC`, job.RunID, req.Name)
		if err != nil {
			fail(err)
			return
		}
		st, err := os.Stat(s.artifactPath(a.ID))
		if err != nil {
			twirpError(w, http.StatusBadRequest, "invalid_argument", "nothing was uploaded for this artifact")
			return
		}
		if req.Size != 0 && st.Size() != req.Size {
			twirpError(w, http.StatusBadRequest, "invalid_argument", fmt.Sprintf("uploaded %d bytes, expected %d", st.Size(), req.Size))
			return
		}
		digest, err := fileDigest(s.artifactPath(a.ID))
		if err != nil {
			fail(err)
			return
		}
		if want := req.GetHash().GetValue(); want != "" && !strings.EqualFold(want, digest) {
			twirpError(w, http.StatusBadRequest, "invalid_argument", "the upload's hash does not match "+want)
			return
		}
		if _, err := s.Store.db.ExecContext(ctx, `UPDATE artifacts SET ready = 1, size = ?, digest = ? WHERE id = ?`,
			st.Size(), digest, a.ID); err != nil {
			fail(err)
			return
		}
		reply(&artifacts.FinalizeArtifactResponse{Ok: true, ArtifactId: a.ID})

	case "ListArtifacts":
		var req artifacts.ListArtifactsRequest
		if !read(&req) {
			return
		}
		as, err := s.Store.Artifacts(ctx, job.RunID)
		if err != nil {
			fail(err)
			return
		}
		// Written by hand: act's message type predates the digest field, which
		// download-artifact checks the download against.
		type listed struct {
			RunID     string `json:"workflowRunBackendId"`
			JobID     string `json:"workflowJobRunBackendId"`
			ID        string `json:"databaseId"`
			Name      string `json:"name"`
			Size      string `json:"size"`
			CreatedAt string `json:"createdAt"`
			Digest    string `json:"digest,omitempty"`
		}
		out := struct {
			Artifacts []listed `json:"artifacts"`
		}{Artifacts: []listed{}}
		for _, a := range as {
			if f := req.GetNameFilter(); f != nil && f.GetValue() != a.Name {
				continue
			}
			if f := req.GetIdFilter(); f != nil && f.GetValue() != a.ID {
				continue
			}
			out.Artifacts = append(out.Artifacts, listed{a.RunID, a.JobID, strconv.FormatInt(a.ID, 10), a.Name,
				strconv.FormatInt(a.Size, 10), a.CreatedAt.UTC().Format(time.RFC3339), a.Digest})
		}
		writeJSON(w, out)

	case "GetSignedArtifactURL":
		var req artifacts.GetSignedArtifactURLRequest
		if !read(&req) {
			return
		}
		a, err := s.Store.artifact(ctx, `WHERE run_id = ? AND name = ? AND ready = 1`, job.RunID, req.Name)
		if err != nil {
			fail(err)
			return
		}
		reply(&artifacts.GetSignedArtifactURLResponse{SignedUrl: s.blobURL(a.ID, "d", time.Now().Add(time.Hour))})

	case "DeleteArtifact":
		var req artifacts.DeleteArtifactRequest
		if !read(&req) {
			return
		}
		a, err := s.Store.artifact(ctx, `WHERE run_id = ? AND name = ? AND ready = 1`, job.RunID, req.Name)
		if err != nil {
			fail(err)
			return
		}
		if err := s.removeArtifact(ctx, a.ID); err != nil {
			fail(err)
			return
		}
		reply(&artifacts.DeleteArtifactResponse{Ok: true, ArtifactId: a.ID})

	default:
		twirpError(w, http.StatusNotFound, "bad_route", "no such method")
	}
}

// uploadBlob takes an artifact's content the way Azure Blob Storage does,
// which is what @actions/artifact uploads to: staged blocks committed by a
// block list, or a single PUT.
func (s *Server) uploadBlob(w http.ResponseWriter, r *http.Request) {
	a, err := s.blobArtifact(r, "u")
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if a.Ready {
		http.Error(w, "the artifact is already finalized", http.StatusConflict)
		return
	}
	q := r.URL.Query()
	switch q.Get("comp") {
	case "block":
		id, err := base64.StdEncoding.DecodeString(q.Get("blockid"))
		if err != nil || len(id) == 0 || len(id) > 64 {
			http.Error(w, "invalid blockid", http.StatusBadRequest)
			return
		}
		if err := os.MkdirAll(s.blockDir(a.ID), 0o755); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := writeFile(filepath.Join(s.blockDir(a.ID), hex.EncodeToString(id)), r.Body); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	case "blocklist":
		var list struct {
			Blocks []struct {
				ID string `xml:",chardata"`
			} `xml:",any"`
		}
		if err := xml.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&list); err != nil {
			http.Error(w, "invalid block list: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.commitBlocks(a.ID, list.Blocks); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.setMime(r, a.ID)
	case "":
		if err := writeFile(s.artifactPath(a.ID), r.Body); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.setMime(r, a.ID)
	default:
		http.Error(w, "unsupported comp="+q.Get("comp"), http.StatusBadRequest)
		return
	}
	h := w.Header()
	h.Set("x-ms-version", "2023-11-03")
	h.Set("x-ms-request-id", newID())
	h.Set("x-ms-request-server-encrypted", "false")
	h.Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
	h.Set("ETag", `"`+newID()+`"`)
	w.WriteHeader(http.StatusCreated)
}

// commitBlocks joins staged blocks, in block list order, into the artifact.
func (s *Server) commitBlocks(id int64, blocks []struct {
	ID string `xml:",chardata"`
}) error {
	dir := s.blockDir(id)
	tmp := s.artifactPath(id) + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	for _, b := range blocks {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b.ID))
		if err != nil {
			f.Close()
			return fmt.Errorf("invalid block id %q", b.ID)
		}
		part, err := os.Open(filepath.Join(dir, hex.EncodeToString(raw)))
		if err != nil {
			f.Close()
			return fmt.Errorf("block %q was never uploaded", b.ID)
		}
		_, err = io.Copy(f, part)
		part.Close()
		if err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	_ = os.RemoveAll(dir)
	return os.Rename(tmp, s.artifactPath(id))
}

func writeFile(path string, r io.Reader) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// serveArtifact sends an artifact: a zip, or a single file uploaded with
// archive: false, which download-artifact saves under the name given here.
func (s *Server) serveArtifact(w http.ResponseWriter, r *http.Request, a *Artifact) {
	f, err := os.Open(s.artifactPath(a.ID))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	mime, name := a.Mime, a.Name
	if mime == "" || mime == "application/zip" {
		mime, name = "application/zip", a.Name+".zip"
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q; filename*=UTF-8''%s`, name, url.PathEscape(name)))
	http.ServeContent(w, r, "", a.CreatedAt, f)
}

// setMime records the content type an upload declared.
func (s *Server) setMime(r *http.Request, id int64) {
	mime := r.Header.Get("x-ms-blob-content-type")
	if mime == "" {
		return
	}
	_, _ = s.Store.db.ExecContext(r.Context(), `UPDATE artifacts SET mime = ? WHERE id = ?`, mime, id)
}

// mimeType reads the mime_type a CreateArtifact request may carry.
func mimeType(body []byte) string {
	var req map[string]json.RawMessage
	if json.Unmarshal(body, &req) != nil {
		return ""
	}
	raw := req["mimeType"]
	if raw == nil {
		raw = req["mime_type"]
	}
	var v string
	if json.Unmarshal(raw, &v) != nil {
		var wrapped struct {
			Value string `json:"value"`
		}
		_ = json.Unmarshal(raw, &wrapped)
		v = wrapped.Value
	}
	return v
}

func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func (s *Server) downloadBlob(w http.ResponseWriter, r *http.Request) {
	a, err := s.blobArtifact(r, "d")
	if err != nil || !a.Ready {
		http.Error(w, "invalid or expired link", http.StatusForbidden)
		return
	}
	s.serveArtifact(w, r, a)
}

// downloadRunArtifact serves an artifact from its run's page. Run ids are
// random, so like job logs these links are unguessable but public.
func (s *Server) downloadRunArtifact(w http.ResponseWriter, r *http.Request) {
	if s.Artifacts == "" {
		http.NotFound(w, r)
		return
	}
	name := strings.TrimSuffix(r.PathValue("name"), ".zip")
	a, err := s.Store.artifact(r.Context(), `WHERE run_id = ? AND name = ? AND ready = 1`, r.PathValue("id"), name)
	if err != nil {
		if a, err = s.Store.artifact(r.Context(), `WHERE run_id = ? AND name = ? AND ready = 1`, r.PathValue("id"), r.PathValue("name")); err != nil {
			http.NotFound(w, r)
			return
		}
	}
	s.serveArtifact(w, r, a)
}
