package server

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/broisnees/selfaction/internal/github"
)

const sha = "1111111111111111111111111111111111111111"

// fakeGitHub serves workflow files and records commit statuses and check runs.
type fakeGitHub struct {
	mu       sync.Mutex
	files    map[string]string
	statuses []map[string]string
	checks   []string // "POST name status" or "PATCH id status conclusion"
	nextID   int64
}

func newFakeGitHub(files map[string]string) *fakeGitHub {
	return &fakeGitHub{files: files}
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	const dir = "/repos/o/app/contents/.github/workflows"
	switch {
	case r.Method == "GET" && r.URL.Path == dir && r.URL.Query().Get("ref") == sha:
		var entries []map[string]string
		for name := range f.files {
			entries = append(entries, map[string]string{"path": ".github/workflows/" + name, "type": "file"})
		}
		entries = append(entries, map[string]string{"path": ".github/workflows/notes.md", "type": "file"})
		_ = json.NewEncoder(w).Encode(entries)
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, dir+"/"):
		fmt.Fprint(w, f.files[strings.TrimPrefix(r.URL.Path, dir+"/")])
	case r.Method == "GET" && r.URL.Path == "/repos/o/app/pulls/7/files":
		fmt.Fprint(w, `[{"filename":"main.go"}]`)
	case r.Method == "POST" && r.URL.Path == "/repos/o/app/statuses/"+sha:
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.statuses = append(f.statuses, body)
		w.WriteHeader(201)
	case r.Method == "POST" && r.URL.Path == "/repos/o/app/check-runs":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.nextID++
		f.checks = append(f.checks, fmt.Sprintf("POST %v %v", body["name"], body["status"]))
		fmt.Fprintf(w, `{"id":%d}`, f.nextID)
	case r.Method == "PATCH" && strings.HasPrefix(r.URL.Path, "/repos/o/app/check-runs/"):
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.checks = append(f.checks, fmt.Sprintf("PATCH %s %v %v", strings.TrimPrefix(r.URL.Path, "/repos/o/app/check-runs/"), body["status"], body["conclusion"]))
		fmt.Fprint(w, `{}`)
	case r.Method == "POST" && r.URL.Path == "/app/installations/99/access_tokens":
		fmt.Fprintf(w, `{"token":"inst-token","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.String(), 404)
	}
}

// last returns the newest status posted for each context.
func (f *fakeGitHub) last() map[string]map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]map[string]string{}
	for _, s := range f.statuses {
		out[s["context"]] = s
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

type harness struct {
	t   *testing.T
	gh  *fakeGitHub
	srv *Server
	api *httptest.Server
}

func newHarness(t *testing.T, files map[string]string, configure func(*Server)) *harness {
	t.Helper()
	gh := newFakeGitHub(files)
	ghSrv := httptest.NewServer(gh)
	t.Cleanup(ghSrv.Close)
	srv := &Server{
		Store: openStore(t), GitHub: &github.Client{API: ghSrv.URL, Token: "gh-token"},
		Secret: "s3cret", Workflows: ".github/workflows", WorkerToken: "wt",
		Log: logrus.New(), PollFor: time.Second,
	}
	if configure != nil {
		configure(srv)
	}
	api := httptest.NewServer(srv.Handler())
	t.Cleanup(api.Close)
	if srv.ExternalURL == "" {
		srv.ExternalURL = api.URL
	}
	if srv.App != nil {
		srv.App.API = ghSrv.URL
	}
	return &harness{t: t, gh: gh, srv: srv, api: api}
}

func (h *harness) send(event, delivery string, body []byte, secret string) int {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	req, _ := http.NewRequest("POST", h.api.URL+"/webhooks/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", delivery)
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

func (h *harness) call(method, path, token string, body any) (int, []byte) {
	var r io.Reader
	if b, ok := body.([]byte); ok {
		r = bytes.NewReader(b)
	} else if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, h.api.URL+path, r)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	return res.StatusCode, out
}

func (h *harness) claim(worker string, labels ...string) *Claim {
	h.t.Helper()
	code, b := h.call("POST", "/api/v1/claim", "wt", map[string]any{"worker": worker, "labels": labels})
	if code != 200 {
		h.t.Fatalf("claim by %s: %d %s", worker, code, b)
	}
	var c Claim
	if err := json.Unmarshal(b, &c); err != nil || c.Job == nil {
		h.t.Fatalf("claim: %s", b)
	}
	return &c
}

func (h *harness) finish(c *Claim, result string, outputs map[string]string) {
	h.t.Helper()
	if code, b := h.call("POST", "/api/v1/jobs/"+c.Job.ID+"/finish", "wt", map[string]any{"worker": c.Job.Worker, "result": result, "outputs": outputs}); code != 200 {
		h.t.Fatalf("finish: %d %s", code, b)
	}
}

func push(files ...string) []byte {
	b, _ := json.Marshal(files)
	return []byte(`{"ref":"refs/heads/main","after":"` + sha + `","repository":{"full_name":"o/app","clone_url":"https://github.com/o/app.git"},
		"installation":{"id":99},"sender":{"login":"me"},"commits":[{"added":[],"removed":[],"modified":` + string(b) + `}]}`)
}

var ciFiles = map[string]string{
	"ci.yml":   "name: ci\non:\n  push:\n    branches: [main]\n  pull_request:\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: 'true'}]\n",
	"docs.yml": "name: docs\non:\n  push:\n    paths: ['docs/**']\njobs:\n  build:\n    runs-on: ubuntu-latest\n    steps: [{run: 'true'}]\n",
}

func TestWebhookToWorkerToStatus(t *testing.T) {
	h := newHarness(t, ciFiles, func(s *Server) { s.ExternalURL = "http://ci.example" })

	// A worker already waiting is woken when the job is queued.
	claimed := make(chan *Claim, 1)
	go func() { claimed <- h.claim("w1", "ubuntu-latest", "self-hosted") }()
	time.Sleep(100 * time.Millisecond)

	if code := h.send("push", "d1", push("main.go"), "wrong"); code != 401 {
		t.Fatalf("bad signature: %d, want 401", code)
	}
	if code := h.send("push", "d1", push("main.go"), "s3cret"); code != 202 {
		t.Fatalf("push: %d, want 202", code)
	}
	if code := h.send("push", "d1", push("main.go"), "s3cret"); code != 200 {
		t.Fatalf("redelivery: %d, want 200 (already handled)", code)
	}
	fork := []byte(`{"action":"opened","repository":{"full_name":"o/app"},"pull_request":{"number":7,
		"head":{"sha":"` + sha + `","ref":"x","repo":{"full_name":"evil/app","clone_url":"https://github.com/evil/app.git"}},"base":{"ref":"main"}}}`)
	h.send("pull_request", "d2", fork, "s3cret")

	var c *Claim
	select {
	case c = <-claimed:
	case <-time.After(5 * time.Second):
		t.Fatal("waiting worker was not woken")
	}
	if c.Job.Name != "test" || c.Job.Key != "test" || c.Job.WorkflowName != "ci" || c.Token != "gh-token" ||
		c.CloneURL != "https://github.com/o/app.git" || !bytes.Contains(c.Event, []byte(`"after":"`+sha)) || c.Env["GITHUB_RUN_ID"] == "" {
		t.Fatalf("claim = %+v job %+v", c, c.Job)
	}
	jobs, _ := h.srv.Store.List(t.Context(), 10)
	if len(jobs) != 1 {
		t.Fatalf("%d jobs queued, want 1: docs.yml's path filter and the fork PR must not queue", len(jobs))
	}

	if code, _ := h.call("POST", "/api/v1/claim", "nope", map[string]any{"worker": "w2"}); code != 401 {
		t.Errorf("claim with a bad token: %d", code)
	}
	if code, _ := h.call("POST", "/api/v1/claim", "wt", map[string]any{"worker": "w2", "labels": []string{"ubuntu-latest"}}); code != 204 {
		t.Errorf("empty queue: %d, want 204", code)
	}

	id := c.Job.ID
	if code, b := h.call("POST", "/api/v1/jobs/"+id+"/log?worker=w1", "wt", []byte("hello\n")); code != 200 || !strings.Contains(string(b), "running") {
		t.Fatalf("log: %d %s", code, b)
	}
	if code, _ := h.call("POST", "/api/v1/jobs/"+id+"/log?worker=w2", "wt", []byte("x")); code != 409 {
		t.Errorf("another worker wrote to the job: %d", code)
	}
	res, _ := http.Get(h.api.URL + "/jobs/" + id + "/log")
	logText, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if string(logText) != "hello\n" {
		t.Errorf("log = %q", logText)
	}
	h.finish(c, Success, nil)
	for _, page := range []string{"/jobs/" + id, "/runs/" + c.Job.RunID} {
		res, _ = http.Get(h.api.URL + page)
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if !strings.Contains(string(b), `class="s success"`) {
			t.Errorf("%s does not show the result:\n%s", page, b)
		}
	}

	const ctx = "selfaction/ci / test (push)"
	eventually(t, "a final status", func() bool { return h.gh.last()[ctx]["state"] == "success" })
	h.gh.mu.Lock()
	defer h.gh.mu.Unlock()
	for i, s := range h.gh.statuses {
		if s["context"] != ctx {
			t.Errorf("status for %q", s["context"])
		}
		if i < len(h.gh.statuses)-1 && s["state"] != "pending" {
			t.Errorf("statuses out of order: %v", h.gh.statuses)
		}
		if s["target_url"] != "http://ci.example/jobs/"+id {
			t.Errorf("status link = %s", s["target_url"])
		}
	}
	if d := h.gh.statuses[len(h.gh.statuses)-1]["description"]; !strings.HasPrefix(d, "Passed in ") {
		t.Errorf("final description %q", d)
	}
}

func TestNeedsAndMatrixAcrossWorkers(t *testing.T) {
	files := map[string]string{"ci.yml": `name: ci
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    outputs: {version: '${{ steps.v.outputs.version }}'}
    steps: [{id: v, run: echo}]
  test:
    needs: build
    strategy:
      matrix:
        os: [ubuntu-latest, macos-latest]
    runs-on: ${{ matrix.os }}
    steps: [{run: echo}]
`}
	h := newHarness(t, files, nil)
	h.send("push", "d1", push("main.go"), "s3cret")

	build := h.claim("linux", "ubuntu-latest")
	if build.Job.Key != "build" || len(build.Needs) != 0 {
		t.Fatalf("first claim = %+v needs %v", build.Job, build.Needs)
	}
	if code, _ := h.call("POST", "/api/v1/claim", "wt", map[string]any{"worker": "mac", "labels": []string{"macos-latest"}}); code != 204 {
		t.Fatalf("the macOS test ran before build finished: %d", code)
	}
	h.finish(build, Success, map[string]string{"version": "1.2.3"})

	mac := h.claim("mac", "macos-latest")
	if mac.Job.Name != "test (macos-latest)" || fmt.Sprint(mac.Job.Matrix) != "map[os:macos-latest]" ||
		mac.Needs["build"].Result != "success" || mac.Needs["build"].Outputs["version"] != "1.2.3" {
		t.Fatalf("mac claim = %+v needs %v", mac.Job, mac.Needs)
	}
	linux := h.claim("linux", "ubuntu-latest")
	if linux.Job.Name != "test (ubuntu-latest)" {
		t.Fatalf("linux claim = %+v", linux.Job)
	}

	// Cancelling the run stops both test jobs.
	if code, b := h.call("POST", "/api/v1/runs/"+mac.Job.RunID+"/cancel", "wt", nil); code != 200 {
		t.Fatalf("cancel run: %d %s", code, b)
	}
	for _, c := range []*Claim{mac, linux} {
		_, b := h.call("POST", "/api/v1/jobs/"+c.Job.ID+"/log?worker="+c.Job.Worker, "wt", []byte("x"))
		if !strings.Contains(string(b), Cancelled) {
			t.Errorf("%s was not told about the cancel: %s", c.Job.Name, b)
		}
	}
	eventually(t, "statuses for all three jobs", func() bool {
		last := h.gh.last()
		return last["selfaction/ci / build (push)"]["state"] == "success" &&
			last["selfaction/ci / test (macos-latest) (push)"]["description"] == "Cancelled" &&
			last["selfaction/ci / test (ubuntu-latest) (push)"]["description"] == "Cancelled"
	})
}

func TestSecretsReachJobsButNotForks(t *testing.T) {
	sealer, err := LoadSealer(filepath.Join(t.TempDir(), "k"))
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, ciFiles, func(s *Server) { s.Sealer = sealer; s.AllowForks = true })
	if code, b := h.call("PUT", "/api/v1/repos/o/app/secrets/npm_token", "wt", []byte("hunter2")); code != 204 {
		t.Fatalf("put secret: %d %s", code, b)
	}
	if code, _ := h.call("PUT", "/api/v1/repos/o/app/secrets/npm_token", "nope", []byte("x")); code != 401 {
		t.Errorf("set a secret without the worker token: %d", code)
	}
	code, b := h.call("GET", "/api/v1/repos/o/app/secrets", "wt", nil)
	if code != 200 || !strings.Contains(string(b), `"NPM_TOKEN"`) || strings.Contains(string(b), "hunter2") {
		t.Fatalf("list secrets: %d %s", code, b)
	}

	h.send("push", "d1", push("main.go"), "s3cret")
	c := h.claim("w", "ubuntu-latest")
	if c.Secrets["NPM_TOKEN"] != "hunter2" {
		t.Errorf("push job secrets = %v", c.Secrets)
	}
	h.finish(c, Success, nil)

	fork := []byte(`{"action":"opened","repository":{"full_name":"o/app"},"pull_request":{"number":7,
		"head":{"sha":"` + sha + `","ref":"x","repo":{"full_name":"evil/app","clone_url":"https://github.com/evil/app.git"}},"base":{"ref":"main"}}}`)
	h.send("pull_request", "d2", fork, "s3cret")
	c = h.claim("w", "ubuntu-latest")
	if c.Job.Event != "pull_request" || len(c.Secrets) != 0 {
		t.Errorf("a fork pull request got secrets: %v", c.Secrets)
	}
	if code, _ := h.call("DELETE", "/api/v1/repos/o/app/secrets/NPM_TOKEN", "wt", nil); code != 204 {
		t.Errorf("delete: %d", code)
	}
}

func TestArtifactsUploadedOnOneWorkerDownloadOnAnother(t *testing.T) {
	files := map[string]string{"ci.yml": "name: ci\non: push\njobs:\n  build:\n    runs-on: a\n    steps: [{run: echo}]\n  test:\n    needs: build\n    runs-on: b\n    steps: [{run: echo}]\n"}
	h := newHarness(t, files, func(s *Server) { s.Artifacts = t.TempDir() })
	h.send("push", "d1", push("main.go"), "s3cret")
	build := h.claim("w1", "a")
	token := build.Env["ACTIONS_RUNTIME_TOKEN"]
	if token == "" || build.Env["ACTIONS_RESULTS_URL"] != h.srv.ExternalURL+"/" {
		t.Fatalf("claim env = %v", build.Env)
	}
	twirp := func(token, method string, body any) (int, map[string]any) {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", h.api.URL+artifactService+"/"+method, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(res.Body).Decode(&out)
		return res.StatusCode, out
	}
	ids := map[string]string{"workflow_run_backend_id": build.Job.RunID, "workflow_job_run_backend_id": build.Job.ID}
	with := func(extra map[string]any) map[string]any {
		m := map[string]any{}
		for k, v := range ids {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}

	code, created := twirp(token, "CreateArtifact", with(map[string]any{"name": "dist", "version": 4}))
	upload, _ := created["signedUploadUrl"].(string)
	if code != 200 || !strings.HasPrefix(upload, h.srv.ExternalURL+"/artifacts/blob/") {
		t.Fatalf("create: %d %v", code, created)
	}
	// Blocks may arrive in any order; the block list sets the order.
	put := func(url string, body []byte) int {
		req, _ := http.NewRequest("PUT", url, bytes.NewReader(body))
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	b1, b2 := base64.StdEncoding.EncodeToString([]byte("block-1")), base64.StdEncoding.EncodeToString([]byte("block-2"))
	if put(upload+"&comp=block&blockid="+b2, []byte("world")) != 201 || put(upload+"&comp=block&blockid="+b1, []byte("hello ")) != 201 {
		t.Fatal("put block")
	}
	if code := put(upload+"&comp=blocklist", []byte(`<?xml version="1.0" encoding="utf-8"?><BlockList><Latest>`+b1+`</Latest><Latest>`+b2+`</Latest></BlockList>`)); code != 201 {
		t.Fatalf("block list: %d", code)
	}
	if code := put(strings.Replace(upload, "sig=", "sig=x", 1), []byte("evil")); code != 403 {
		t.Errorf("upload with a bad signature: %d", code)
	}
	if code, _ := twirp(token, "FinalizeArtifact", with(map[string]any{"name": "dist", "size": "11", "hash": "sha256:abc"})); code != 400 {
		t.Errorf("finalize with the wrong hash: %d", code)
	}
	sum := sha256.Sum256([]byte("hello world"))
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if code, out := twirp(token, "FinalizeArtifact", with(map[string]any{"name": "dist", "size": "11", "hash": digest})); code != 200 || out["ok"] != true {
		t.Fatalf("finalize: %d %v", code, out)
	}

	// archive: false uploads one file as is, with its own content type.
	code, created = twirp(token, "CreateArtifact", with(map[string]any{"name": "app.apk", "mimeType": "application/vnd.android.package-archive"}))
	if code != 200 {
		t.Fatalf("create app.apk: %d %v", code, created)
	}
	req, _ := http.NewRequest("PUT", created["signedUploadUrl"].(string), strings.NewReader("APK"))
	req.Header.Set("x-ms-blob-type", "BlockBlob")
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != 201 {
		t.Fatalf("single put: %v %v", res, err)
	}
	if code, out := twirp(token, "FinalizeArtifact", with(map[string]any{"name": "app.apk", "size": "3"})); code != 200 {
		t.Fatalf("finalize app.apk: %d %v", code, out)
	}
	if code, _ := twirp(token, "CreateArtifact", with(map[string]any{"name": "dist"})); code != 409 {
		t.Errorf("a second artifact with the same name: %d", code)
	}
	h.finish(build, Success, nil)
	if code, _ := twirp(token, "ListArtifacts", ids); code != 401 {
		t.Errorf("a finished job's token still works: %d", code)
	}

	test := h.claim("w2", "b")
	token2 := test.Env["ACTIONS_RUNTIME_TOKEN"]
	ids["workflow_job_run_backend_id"] = test.Job.ID
	code, list := twirp(token2, "ListArtifacts", ids)
	arts, _ := list["artifacts"].([]any)
	if code != 200 || len(arts) != 2 || arts[1].(map[string]any)["name"] != "dist" || arts[1].(map[string]any)["size"] != "11" ||
		arts[1].(map[string]any)["digest"] != digest {
		t.Fatalf("list: %d %v", code, list)
	}
	code, signedAPK := twirp(token2, "GetSignedArtifactURL", with(map[string]any{"name": "app.apk"}))
	res, err := http.Get(signedAPK["signedUrl"].(string))
	if err != nil || code != 200 {
		t.Fatal(code, err)
	}
	apk, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if string(apk) != "APK" || res.Header.Get("Content-Type") != "application/vnd.android.package-archive" ||
		!strings.Contains(res.Header.Get("Content-Disposition"), `filename="app.apk"`) {
		t.Errorf("unzipped artifact = %q %v", apk, res.Header)
	}
	code, signed := twirp(token2, "GetSignedArtifactURL", with(map[string]any{"name": "dist"}))
	if code != 200 {
		t.Fatalf("signed url: %d %v", code, signed)
	}
	res, err = http.Get(signed["signedUrl"].(string))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if string(got) != "hello world" || res.Header.Get("Content-Type") != "application/zip" {
		t.Errorf("download = %q (%s)", got, res.Header.Get("Content-Type"))
	}

	other := map[string]any{"workflow_run_backend_id": "another-run", "workflow_job_run_backend_id": test.Job.ID}
	if code, _ := twirp(token2, "ListArtifacts", other); code != 403 {
		t.Errorf("listed another run's artifacts: %d", code)
	}

	res, _ = http.Get(h.api.URL + "/runs/" + test.Job.RunID)
	page, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(string(page), "dist") {
		t.Errorf("the run page does not link the artifact")
	}
	res, _ = http.Get(h.api.URL + "/runs/" + test.Job.RunID + "/artifacts/dist")
	got, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if string(got) != "hello world" || !strings.Contains(res.Header.Get("Content-Disposition"), "dist.zip") {
		t.Errorf("run page download = %q %v", got, res.Header)
	}

	// Expired artifacts are removed.
	_, _ = h.srv.Store.db.Exec(`UPDATE artifacts SET expires_at = 1`)
	if n, err := h.srv.expireArtifacts(t.Context()); err != nil || n != 2 {
		t.Errorf("expire = %d, %v", n, err)
	}
}

func TestGitHubAppReportsCheckRuns(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, ciFiles, func(s *Server) { s.App = &github.App{ID: 1, Key: key} })
	h.send("push", "d1", push("main.go"), "s3cret")
	c := h.claim("w", "ubuntu-latest")
	if c.Token != "inst-token" {
		t.Errorf("job token = %q, want the installation token", c.Token)
	}
	h.finish(c, Success, nil)
	eventually(t, "a completed check run", func() bool {
		h.gh.mu.Lock()
		defer h.gh.mu.Unlock()
		n := len(h.gh.checks)
		return n > 0 && strings.Contains(h.gh.checks[n-1], "completed success")
	})
	h.gh.mu.Lock()
	defer h.gh.mu.Unlock()
	posts := 0
	for _, c := range h.gh.checks {
		if strings.HasPrefix(c, "POST") {
			posts++
			if c != "POST ci / test (push) queued" && c != "POST ci / test (push) in_progress" {
				t.Errorf("created %s", c)
			}
		} else if !strings.HasPrefix(c, "PATCH 1 ") {
			t.Errorf("updated another check run: %s", c)
		}
	}
	if posts != 1 || len(h.gh.statuses) != 0 {
		t.Errorf("checks = %v, statuses = %v", h.gh.checks, h.gh.statuses)
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
