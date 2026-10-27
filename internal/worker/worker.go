// Package worker pulls jobs from a selfaction server and runs them with the
// act engine: clone the commit, run the workflow with the event's payload,
// stream the log back, report the result.
package worker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/broisnees/selfaction/internal/cache"
	"github.com/broisnees/selfaction/internal/run"
	"github.com/broisnees/selfaction/internal/server"
)

type Worker struct {
	Server    string   // server base URL
	Token     string   // the server's worker token
	Name      string   // slot n reports as <Name>-<n>
	Labels    []string // runs-on labels this worker serves
	Slots     int
	Dir       string        // workspaces, local cache and artifacts
	Platforms []string      // runs-on label=image overrides, as for selfaction run
	Arch      string        // container platform
	Timeout   time.Duration // per job; 0 = 6h, GitHub's limit
	Log       logrus.FieldLogger
	HTTP      *http.Client

	// Execute runs a workflow; run.Run unless a test replaces it.
	Execute func(context.Context, run.Options) error
}

// FlushEvery is how often a running job's output is sent; it is also the heartbeat.
var FlushEvery = 2 * time.Second

func (w *Worker) Run(ctx context.Context) error {
	if w.Execute == nil {
		w.Execute = run.Run
	}
	if w.HTTP == nil {
		w.HTTP = &http.Client{}
	}
	if w.Slots < 1 {
		w.Slots = 1
	}
	w.Log.Infof("serving %s with %d slot(s), labels %s", w.Server, w.Slots, strings.Join(w.Labels, ","))
	var wg sync.WaitGroup
	for n := 1; n <= w.Slots; n++ {
		wg.Go(func() { w.slot(ctx, fmt.Sprintf("%s-%d", w.Name, n)) })
	}
	wg.Wait()
	return nil
}

func (w *Worker) slot(ctx context.Context, name string) {
	backoff := time.Second
	for ctx.Err() == nil {
		c, err := w.claim(ctx, name)
		if err != nil {
			if ctx.Err() == nil {
				w.Log.Warnf("%s: claim: %v (retrying in %s)", name, err, backoff)
				select {
				case <-ctx.Done():
				case <-time.After(backoff):
				}
				backoff = min(backoff*2, 30*time.Second)
			}
			continue
		}
		backoff = time.Second
		if c != nil {
			w.work(ctx, name, c)
		}
	}
}

// claim long-polls the server; nil means no work arrived in time.
func (w *Worker) claim(ctx context.Context, name string) (*server.Claim, error) {
	body, _ := json.Marshal(map[string]any{"worker": name, "labels": w.Labels})
	pctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	res, err := w.post(pctx, "/api/v1/claim", "application/json", body)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	var c server.Claim
	if err := json.NewDecoder(res.Body).Decode(&c); err != nil {
		return nil, err
	}
	return &c, nil
}

// work runs one job to completion and reports the result.
func (w *Worker) work(ctx context.Context, name string, c *server.Claim) {
	j := c.Job
	timeout := w.Timeout
	if timeout == 0 {
		timeout = 6 * time.Hour
	}
	jctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	w.Log.Infof("%s: running %s for %s %s@%.7s (job %s)", name, j.Name, j.Event, j.Repo, j.SHA, j.ID)

	out := &stream{}
	cancelled := make(chan struct{})
	flushed := make(chan struct{})
	go func() {
		defer close(flushed)
		w.ship(jctx, name, j.ID, out, cancel, cancelled)
	}()

	result := server.Success
	if err := w.execute(jctx, c, out); err != nil {
		fmt.Fprintf(out, "\n%s\n", err)
		result = server.Failure
	}
	select {
	case <-cancelled:
		result = server.Cancelled
	default:
		switch {
		case ctx.Err() != nil:
			fmt.Fprintln(out, "\nThe worker was stopped.")
			result = server.Cancelled
		case errors.Is(jctx.Err(), context.DeadlineExceeded):
			fmt.Fprintf(out, "\nThe job exceeded the %s time limit.\n", timeout)
		}
	}
	cancel()
	<-flushed
	if _, err := w.send(ctx, name, j.ID, out.take()); err != nil { // whatever arrived after the last flush
		w.Log.Warnf("%s: send the end of job %s's log: %v", name, j.ID, err)
	}

	select {
	case <-cancelled: // the server already ended the job; finishing it again only gets a 409
	default:
		// ctx is done when the worker is stopping; the result must still reach the server.
		fctx, stop := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer stop()
		body, _ := json.Marshal(map[string]string{"worker": name, "result": result})
		if res, err := w.post(fctx, "/api/v1/jobs/"+j.ID+"/finish", "application/json", body); err != nil {
			w.Log.Errorf("%s: report result of job %s: %v", name, j.ID, err)
		} else {
			res.Body.Close()
		}
	}
	w.Log.Infof("%s: job %s %s", name, j.ID, result)
}

// execute clones the commit and runs the workflow in a scratch directory.
func (w *Worker) execute(ctx context.Context, c *server.Claim, out io.Writer) error {
	j := c.Job
	dir := filepath.Join(w.Dir, "jobs", j.ID)
	defer os.RemoveAll(dir)
	defer os.Remove(dir + ".event.json")
	fmt.Fprintf(out, "Fetching %s at %.7s\n", j.Repo, j.SHA)
	if err := fetch(ctx, dir, j.CloneURL, j.SHA, j.Ref, c.Token, out); err != nil {
		return fmt.Errorf("fetch %s: %w", j.Repo, err)
	}
	event := dir + ".event.json"
	if err := os.WriteFile(event, c.Event, 0o600); err != nil {
		return err
	}
	return w.Execute(ctx, run.Options{
		Dir:         dir,
		Workflows:   j.Workflow,
		Event:       j.Event,
		EventPath:   event,
		Secrets:     []string{"GITHUB_TOKEN=" + c.Token},
		Platforms:   w.Platforms,
		Arch:        w.Arch,
		CacheURL:    c.CacheURL,
		CacheDir:    filepath.Join(w.Dir, "cache"),
		CachePolicy: cache.Policy{TTL: 7 * 24 * time.Hour, MaxScopeBytes: 10 << 30},
		ArtifactDir: filepath.Join(w.Dir, "artifacts", j.ID),
		Log:         out,
	})
}

// fetch makes dir a shallow clone of exactly sha. The token travels in the
// environment, so it is neither written to .git/config nor visible in ps.
func fetch(ctx context.Context, dir, url, sha, ref, token string, out io.Writer) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	env := os.Environ()
	if token != "" {
		basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
		env = append(env, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0=Authorization: Basic "+basic)
	}
	git := func(args ...string) error {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = dir, env, out, out
		return cmd.Run()
	}
	checkout := []string{"checkout", "-q", "--detach", "FETCH_HEAD"}
	if branch, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
		checkout = []string{"checkout", "-q", "-B", branch, "FETCH_HEAD"} // act reads the branch from HEAD
	}
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", url}, {"fetch", "-q", "--depth", "1", "origin", sha}, checkout} {
		if err := git(args...); err != nil {
			return fmt.Errorf("git %s: %w", args[0], err)
		}
	}
	return nil
}

// ship sends the job's output every FlushEvery until ctx ends. Each send is
// also the heartbeat; if the server says the job is no longer running, the
// job is cancelled.
func (w *Worker) ship(ctx context.Context, name, id string, out *stream, cancel context.CancelFunc, cancelled chan<- struct{}) {
	t := time.NewTicker(FlushEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		chunk := out.take()
		status, err := w.send(ctx, name, id, chunk)
		if err != nil {
			w.Log.Warnf("%s: send log: %v", name, err)
			out.unshift(chunk)
			continue
		}
		if status != server.Running {
			w.Log.Infof("%s: job %s is %s on the server; stopping it", name, id, status)
			close(cancelled)
			cancel()
			return
		}
	}
}

// send posts a chunk of output and returns the job's status on the server.
func (w *Worker) send(ctx context.Context, name, id string, chunk []byte) (string, error) {
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	res, err := w.post(pctx, "/api/v1/jobs/"+id+"/log?worker="+name, "text/plain", chunk)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	var r struct {
		Status string `json:"status"`
	}
	return r.Status, json.NewDecoder(res.Body).Decode(&r)
}

func (w *Worker) post(ctx context.Context, path, contentType string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimSuffix(w.Server, "/")+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+w.Token)
	req.Header.Set("Content-Type", contentType)
	res, err := w.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 300 && res.StatusCode != http.StatusNoContent {
		defer res.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return nil, fmt.Errorf("%s: %s %s", path, res.Status, bytes.TrimSpace(b))
	}
	return res, nil
}

// stream collects output from concurrent jobs of one workflow.
type stream struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *stream) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

// unshift puts back a chunk that could not be sent, ahead of newer output.
func (s *stream) unshift(b []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rest := bytes.Clone(s.buf.Bytes())
	s.buf.Reset()
	s.buf.Write(b)
	s.buf.Write(rest)
}

func (s *stream) take() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := bytes.Clone(s.buf.Bytes())
	s.buf.Reset()
	return b
}
