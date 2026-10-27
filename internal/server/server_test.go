package server

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/broisnees/selfaction/internal/github"
)

const sha = "1111111111111111111111111111111111111111"

// fakeGitHub serves workflow files and records commit statuses.
type fakeGitHub struct {
	mu       sync.Mutex
	statuses []map[string]string
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	files := map[string]string{
		"ci.yml":   "name: ci\non:\n  push:\n    branches: [main]\n  pull_request:\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: 'true'}]\n",
		"docs.yml": "name: docs\non:\n  push:\n    paths: ['docs/**']\njobs:\n  build:\n    runs-on: ubuntu-latest\n    steps: [{run: 'true'}]\n",
	}
	switch {
	case r.Method == "GET" && r.URL.Path == "/repos/o/app/contents/.github/workflows" && r.URL.Query().Get("ref") == sha:
		fmt.Fprint(w, `[{"path":".github/workflows/ci.yml","type":"file"},{"path":".github/workflows/docs.yml","type":"file"},{"path":".github/workflows/notes.md","type":"file"}]`)
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/repos/o/app/contents/.github/workflows/"):
		fmt.Fprint(w, files[strings.TrimPrefix(r.URL.Path, "/repos/o/app/contents/.github/workflows/")])
	case r.Method == "GET" && r.URL.Path == "/repos/o/app/pulls/7/files":
		fmt.Fprint(w, `[{"filename":"main.go"}]`)
	case r.Method == "POST" && r.URL.Path == "/repos/o/app/statuses/"+sha:
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.statuses = append(f.statuses, body)
		f.mu.Unlock()
		w.WriteHeader(201)
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.String(), 404)
	}
}

func (f *fakeGitHub) states() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, s := range f.statuses {
		out = append(out, s["context"]+"="+s["state"]+":"+s["description"])
	}
	return out
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestWebhookToWorkerToStatus(t *testing.T) {
	gh := &fakeGitHub{}
	ghSrv := httptest.NewServer(gh)
	defer ghSrv.Close()
	srv := &Server{
		Store: openStore(t), GitHub: &github.Client{API: ghSrv.URL, Token: "gh-token"},
		Secret: "s3cret", Workflows: ".github/workflows", ExternalURL: "http://ci.example", WorkerToken: "wt",
		Log: logrus.New(), PollFor: time.Second,
	}
	api := httptest.NewServer(srv.Handler())
	defer api.Close()

	send := func(event, delivery string, body []byte, secret string) int {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		req, _ := http.NewRequest("POST", api.URL+"/webhooks/github", bytes.NewReader(body))
		req.Header.Set("X-GitHub-Event", event)
		req.Header.Set("X-GitHub-Delivery", delivery)
		req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	call := func(method, path, token string, body any) (int, []byte) {
		var r io.Reader
		if b, ok := body.([]byte); ok {
			r = bytes.NewReader(b)
		} else if body != nil {
			b, _ := json.Marshal(body)
			r = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, api.URL+path, r)
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		out, _ := io.ReadAll(res.Body)
		return res.StatusCode, out
	}

	// A worker already waiting is woken when the job is queued.
	claimed := make(chan []byte, 1)
	go func() {
		_, b := call("POST", "/api/v1/claim", "wt", map[string]any{"worker": "w1", "labels": []string{"ubuntu-latest", "self-hosted"}})
		claimed <- b
	}()
	time.Sleep(100 * time.Millisecond)

	push := []byte(`{"ref":"refs/heads/main","after":"` + sha + `","repository":{"full_name":"o/app","clone_url":"https://github.com/o/app.git"},
		"commits":[{"added":[],"removed":[],"modified":["main.go"]}]}`)
	if code := send("push", "d1", push, "wrong"); code != 401 {
		t.Fatalf("bad signature: %d, want 401", code)
	}
	if code := send("push", "d1", push, "s3cret"); code != 202 {
		t.Fatalf("push: %d, want 202", code)
	}
	if code := send("push", "d1", push, "s3cret"); code != 200 {
		t.Fatalf("redelivery: %d, want 200 (already handled)", code)
	}
	fork := []byte(`{"action":"opened","repository":{"full_name":"o/app"},"pull_request":{"number":7,
		"head":{"sha":"` + sha + `","ref":"x","repo":{"full_name":"evil/app","clone_url":"https://github.com/evil/app.git"}},"base":{"ref":"main"}}}`)
	send("pull_request", "d2", fork, "s3cret")

	var c Claim
	select {
	case b := <-claimed:
		if err := json.Unmarshal(b, &c); err != nil || c.Job == nil {
			t.Fatalf("claim: %s", b)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiting worker was not woken")
	}
	if c.Job.Name != "ci" || c.Token != "gh-token" || !bytes.Contains(c.Event, []byte(`"after":"`+sha)) {
		t.Fatalf("claim = %+v event %s", c.Job, c.Event)
	}
	jobs, _ := srv.Store.List(t.Context(), 10)
	if len(jobs) != 1 {
		t.Fatalf("%d jobs queued, want 1: docs.yml's path filter and the fork PR must not queue", len(jobs))
	}

	if code, _ := call("POST", "/api/v1/claim", "nope", map[string]any{"worker": "w2"}); code != 401 {
		t.Errorf("claim with a bad token: %d", code)
	}
	if code, _ := call("POST", "/api/v1/claim", "wt", map[string]any{"worker": "w2", "labels": []string{"ubuntu-latest"}}); code != 204 {
		t.Errorf("empty queue: %d, want 204", code)
	}

	id := c.Job.ID
	if code, b := call("POST", "/api/v1/jobs/"+id+"/log?worker=w1", "wt", []byte("hello\n")); code != 200 || !strings.Contains(string(b), "running") {
		t.Fatalf("log: %d %s", code, b)
	}
	if code, _ := call("POST", "/api/v1/jobs/"+id+"/log?worker=w2", "wt", []byte("x")); code != 409 {
		t.Errorf("another worker wrote to the job: %d", code)
	}
	res, _ := http.Get(api.URL + "/jobs/" + id + "/log")
	logText, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if string(logText) != "hello\n" {
		t.Errorf("log = %q", logText)
	}
	if code, b := call("POST", "/api/v1/jobs/"+id+"/finish", "wt", map[string]string{"worker": "w1", "result": "success"}); code != 200 {
		t.Fatalf("finish: %d %s", code, b)
	}
	res, _ = http.Get(api.URL + "/jobs/" + id)
	page, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(string(page), `class="s success"`) || !strings.Contains(string(page), "hello") {
		t.Errorf("job page does not show the result and log")
	}

	want := []string{"selfaction/ci (push)=pending:Queued", "selfaction/ci (push)=pending:Running on w1", "selfaction/ci (push)=success:Passed in "}
	eventually(t, "three statuses", func() bool { return len(gh.states()) == 3 })
	got := gh.states()
	for _, w := range want {
		if !strings.Contains(strings.Join(got, "\n"), w) {
			t.Errorf("missing status %q in %v", w, got)
		}
	}
	gh.mu.Lock()
	link := gh.statuses[0]["target_url"]
	gh.mu.Unlock()
	if link != "http://ci.example/jobs/"+id {
		t.Errorf("status link = %s", link)
	}
}

func TestCancelReachesTheWorker(t *testing.T) {
	gh := &fakeGitHub{}
	ghSrv := httptest.NewServer(gh)
	defer ghSrv.Close()
	srv := &Server{Store: openStore(t), GitHub: &github.Client{API: ghSrv.URL}, WorkerToken: "wt", Log: logrus.New()}
	j := &Job{Repo: "o/app", SHA: sha, Name: "ci", Event: "push", Payload: []byte("{}")}
	_ = srv.Store.Enqueue(t.Context(), j)
	_, _ = srv.Store.Claim(t.Context(), "w1", nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/jobs/"+j.ID+"/cancel", nil)
	req.Header.Set("Authorization", "Bearer wt")
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("cancel: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/v1/jobs/"+j.ID+"/log?worker=w1", strings.NewReader("more"))
	req.Header.Set("Authorization", "Bearer wt")
	srv.Handler().ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), Cancelled) {
		t.Errorf("worker was not told about the cancel: %s", rec.Body)
	}
	if b, _ := srv.Store.Log(j.ID); len(b) != 0 {
		t.Errorf("output after cancel was stored: %q", b)
	}
}

func TestSignature(t *testing.T) {
	body := []byte(`{"zen":"ok"}`)
	mac := hmac.New(sha256.New, []byte("k"))
	mac.Write(body)
	good := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if !validSignature("k", body, good) {
		t.Error("valid signature rejected")
	}
	for _, bad := range []string{"", "sha1=abc", good[:len(good)-2] + "00", strings.TrimPrefix(good, "sha256=")} {
		if validSignature("k", body, bad) {
			t.Errorf("accepted %q", bad)
		}
	}
	if validSignature("", body, good) {
		t.Error("an empty secret must refuse every delivery")
	}
}
