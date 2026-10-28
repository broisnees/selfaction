package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/nektos/act/pkg/model"

	"github.com/broisnees/selfaction/internal/github"
)

// GitHub caps webhook payloads at 25 MB.
const maxPayload = 25 << 20

// webhook verifies a delivery, answers at once, and plans the run in the
// background: GitHub gives up on deliveries that take longer than 10 seconds.
func (s *Server) webhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxPayload))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !validSignature(s.Secret, body, r.Header.Get("X-Hub-Signature-256")) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	event := r.Header.Get("X-GitHub-Event")
	if event == "ping" {
		fmt.Fprintln(w, "pong")
		return
	}
	if event != "push" && event != "pull_request" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if id := r.Header.Get("X-GitHub-Delivery"); id != "" {
		if seen, err := s.Store.SeenDelivery(r.Context(), id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		} else if seen {
			fmt.Fprintln(w, "already handled")
			return
		}
	}
	w.WriteHeader(http.StatusAccepted)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := s.plan(ctx, event, body); err != nil {
			s.Log.Errorf("%s webhook: %v", event, err)
		}
	}()
}

func validSignature(secret string, body []byte, header string) bool {
	if secret == "" {
		return false
	}
	got, err := hex.DecodeString(strings.TrimPrefix(header, "sha256="))
	if err != nil || !strings.HasPrefix(header, "sha256=") {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// payload holds the fields of push and pull_request deliveries the server uses.
type payload struct {
	Ref          string `json:"ref"`
	After        string `json:"after"`
	Deleted      bool   `json:"deleted"`
	Action       string `json:"action"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Repository struct {
		FullName string `json:"full_name"`
		CloneURL string `json:"clone_url"`
	} `json:"repository"`
	Commits []struct {
		Added    []string `json:"added"`
		Removed  []string `json:"removed"`
		Modified []string `json:"modified"`
	} `json:"commits"`
	PullRequest *struct {
		Number int `json:"number"`
		Head   struct {
			SHA  string `json:"sha"`
			Ref  string `json:"ref"`
			Repo struct {
				FullName string `json:"full_name"`
				CloneURL string `json:"clone_url"`
			} `json:"repo"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	} `json:"pull_request"`
}

// plan turns one event into runs, one per workflow it triggers.
func (s *Server) plan(ctx context.Context, event string, body []byte) error {
	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		return err
	}
	repo := p.Repository.FullName
	run := Run{Repo: repo, Event: event, Payload: body, CloneURL: p.Repository.CloneURL, Installation: p.Installation.ID}
	gh, err := s.client(ctx, run.Installation)
	if err != nil {
		return fmt.Errorf("%s: %w", repo, err)
	}
	t := trigger{event: event}

	switch event {
	case "push":
		if p.Deleted || strings.Trim(p.After, "0") == "" {
			return nil
		}
		run.SHA, run.Ref, t.ref = p.After, p.Ref, p.Ref
		// GitHub lists at most 2048 commits; beyond that the changes are unknown.
		if n := len(p.Commits); n > 0 && n < 2048 {
			t.files = []string{}
			for _, c := range p.Commits {
				t.files = append(append(append(t.files, c.Added...), c.Removed...), c.Modified...)
			}
		}
	case "pull_request":
		pr := p.PullRequest
		if pr == nil {
			return nil
		}
		run.Fork = !strings.EqualFold(pr.Head.Repo.FullName, repo)
		if run.Fork && !s.AllowForks {
			s.Log.Infof("%s#%d: skipping a pull request from fork %s", repo, pr.Number, pr.Head.Repo.FullName)
			return nil
		}
		run.SHA, run.Ref, run.CloneURL = pr.Head.SHA, "refs/heads/"+pr.Head.Ref, pr.Head.Repo.CloneURL
		t.action, t.ref = p.Action, "refs/heads/"+pr.Base.Ref
		t.files = s.pullFiles(ctx, gh, repo, pr.Number)
	}

	files, err := s.workflowFiles(ctx, gh, repo, run.SHA)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		src := files[name]
		wf, err := model.ReadWorkflow(bytes.NewReader(src), false)
		if err != nil {
			s.Log.Warnf("%s@%.7s: %s: %v", repo, run.SHA, name, err)
			continue
		}
		ok, err := t.matches(wf)
		if err != nil {
			s.Log.Warnf("%s: %v", name, err)
		}
		if !ok {
			continue
		}
		if err := validateNeeds(wf); err != nil {
			s.Log.Warnf("%s@%.7s: %s: %v", repo, run.SHA, name, err)
			continue
		}
		r := run
		r.Workflow, r.Name, r.Source = name, workflowName(wf, name), src
		if err := s.startRun(ctx, &r); err != nil {
			return err
		}
		s.Log.Infof("run %s: %s for %s %s@%.7s", r.ID, r.Name, event, repo, r.SHA)
	}
	return nil
}

// workflowFiles returns the workflow files at sha, by path.
func (s *Server) workflowFiles(ctx context.Context, gh *github.Client, repo, sha string) (map[string][]byte, error) {
	var entries []struct {
		Path string `json:"path"`
		Type string `json:"type"`
	}
	dir := fmt.Sprintf("/repos/%s/contents/%s?ref=%s", repo, escapePath(s.Workflows), url.QueryEscape(sha))
	if err := gh.Do(ctx, "GET", dir, nil, &entries); err != nil {
		if github.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list %s: %w", s.Workflows, err)
	}
	out := map[string][]byte{}
	for _, e := range entries {
		if e.Type != "file" || (path.Ext(e.Path) != ".yml" && path.Ext(e.Path) != ".yaml") {
			continue
		}
		src, err := gh.Raw(ctx, fmt.Sprintf("/repos/%s/contents/%s?ref=%s", repo, escapePath(e.Path), url.QueryEscape(sha)),
			"application/vnd.github.raw+json", 1<<20)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", e.Path, err)
		}
		out[e.Path] = src
	}
	return out, nil
}

// pullFiles lists a pull request's changed paths, or nil if they cannot be
// listed in full (path filters are then skipped rather than guessed).
func (s *Server) pullFiles(ctx context.Context, gh *github.Client, repo string, number int) []string {
	files := []string{}
	for page := 1; page <= 30; page++ { // GitHub lists at most 3000 files
		var batch []struct {
			Filename string `json:"filename"`
		}
		if err := gh.Do(ctx, "GET", fmt.Sprintf("/repos/%s/pulls/%d/files?per_page=100&page=%d", repo, number, page), nil, &batch); err != nil {
			s.Log.Warnf("%s#%d: list changed files: %v", repo, number, err)
			return nil
		}
		for _, f := range batch {
			files = append(files, f.Filename)
		}
		if len(batch) < 100 {
			return files
		}
	}
	return nil
}

func escapePath(p string) string {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}
