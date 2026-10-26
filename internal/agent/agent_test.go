package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/sirupsen/logrus"
)

// fakeGitHub records runner registrations and deletions.
type fakeGitHub struct {
	mu      sync.Mutex
	created []map[string]any
	deleted []string
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer tkn" {
		http.Error(w, `{"message":"Bad credentials"}`, 401)
		return
	}
	switch {
	case r.Method == "POST" && r.URL.Path == "/repos/octo/app/actions/runners/generate-jitconfig":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.created = append(f.created, body)
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(map[string]any{"runner": map[string]any{"id": 42, "name": body["name"]}, "encoded_jit_config": "SECRET-JIT"})
	case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/repos/octo/app/actions/runners/"):
		f.deleted = append(f.deleted, r.URL.Path)
		http.Error(w, `{"message":"Not Found"}`, 404) // already removed itself: must not be an error
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, 500)
	}
}

// TestRunJobHostMode runs one slot cycle against a fake GitHub and a fake
// run.sh: the JIT config must reach the runner via env, the slot must be
// cleaned (keeping the tool cache) and the registration removed.
func TestRunJobHostMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("host mode uses run.sh")
	}
	gh := &fakeGitHub{}
	srv := httptest.NewServer(gh)
	defer srv.Close()

	var logs bytes.Buffer
	log := logrus.New()
	log.Out = &logs
	a := &Agent{
		Config:  Config{Target: "octo/app", Name: "box", Mode: "host", GroupID: 1, Dir: t.TempDir(), Labels: []string{"gpu", "GPU", "self-hosted"}},
		GitHub:  &GitHub{API: srv.URL, Token: "tkn"},
		Log:     log,
		version: "2.0.0",
	}
	a.labels = a.defaultLabels()
	dir := a.slotDir(1)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho \"got=$ACTIONS_RUNNER_INPUT_JITCONFIG trap=$RUNNER_MANUALLY_TRAP_SIG\"\nmkdir -p _work/octo/app _work/_tool/node _diag\ntouch .runner .credentials\n"
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := a.runJob(context.Background(), 1); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(logs.String(), "got=SECRET-JIT trap=1") {
		t.Errorf("runner did not receive the JIT config; logs:\n%s", logs.String())
	}
	for path, want := range map[string]bool{"_work/octo/app": false, "_diag": false, ".credentials": false, ".runner": false, "_work/_tool/node": true} {
		if _, err := os.Stat(filepath.Join(dir, path)); (err == nil) != want {
			t.Errorf("%s exists=%v, want %v", path, err == nil, want)
		}
	}
	if len(gh.created) != 1 || len(gh.deleted) != 1 || gh.deleted[0] != "/repos/octo/app/actions/runners/42" {
		t.Fatalf("created %v deleted %v", gh.created, gh.deleted)
	}
	labels, _ := json.Marshal(gh.created[0]["labels"])
	wantOS := map[string]string{"darwin": "macOS", "linux": "Linux"}[runtime.GOOS]
	if !strings.HasPrefix(string(labels), `["self-hosted","`+wantOS+`",`) || !strings.HasSuffix(string(labels), `,"gpu"]`) {
		t.Errorf("labels = %s (want self-hosted, OS, arch, then deduped extras)", labels)
	}
	if !strings.HasPrefix(gh.created[0]["name"].(string), "box-1-") {
		t.Errorf("name = %v", gh.created[0]["name"])
	}
}

// TestPrepareHostRealRelease downloads, verifies and unpacks the real
// actions/runner release and runs its binary. Network-heavy, so opt-in:
// SELFACTION_E2E=1 go test ./internal/agent -run Real
func TestPrepareHostRealRelease(t *testing.T) {
	if os.Getenv("SELFACTION_E2E") == "" {
		t.Skip("set SELFACTION_E2E=1")
	}
	a := &Agent{Config: Config{Slots: 2, Dir: t.TempDir()}, Log: logrus.New()}
	if err := a.prepareHost(context.Background()); err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= 2; n++ {
		out, err := exec.Command(filepath.Join(a.slotDir(n), "bin", "Runner.Listener"), "--version").CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != a.version {
			t.Fatalf("slot %d: Runner.Listener --version = %q, %v (want %s)", n, out, err, a.version)
		}
	}
}

func TestExtract(t *testing.T) {
	mk := func(entries ...[3]string) string { // name, type (f/d/l), content or link target
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		for _, e := range entries {
			h := &tar.Header{Name: e[0], Mode: 0o755}
			switch e[1] {
			case "d":
				h.Typeflag = tar.TypeDir
			case "l":
				h.Typeflag, h.Linkname = tar.TypeSymlink, e[2]
			default:
				h.Typeflag, h.Size = tar.TypeReg, int64(len(e[2]))
			}
			_ = tw.WriteHeader(h)
			_, _ = tw.Write([]byte(e[2]))
		}
		tw.Close()
		gz.Close()
		p := filepath.Join(t.TempDir(), "r.tar.gz")
		_ = os.WriteFile(p, buf.Bytes(), 0o644)
		return p
	}

	dest := filepath.Join(t.TempDir(), "slot-1")
	good := mk([3]string{"./", "d", ""}, [3]string{"./bin/", "d", ""}, [3]string{"./run.sh", "f", "#!/bin/sh"}, [3]string{"./bin/node", "l", "../run.sh"})
	if err := extract(good, dest); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(filepath.Join(dest, "run.sh")); err != nil || st.Mode().Perm() != 0o755 {
		t.Errorf("run.sh: %v %v", st, err)
	}
	if target, _ := os.Readlink(filepath.Join(dest, "bin", "node")); target != "../run.sh" {
		t.Errorf("symlink = %q", target)
	}

	evil := mk([3]string{"../../escape.sh", "f", "boom"})
	if err := extract(evil, filepath.Join(t.TempDir(), "slot-2")); err == nil {
		t.Error("archive entry escaping the destination was accepted")
	}
}

func TestLineLoggerDemotesRunnerDiagnostics(t *testing.T) {
	var buf bytes.Buffer
	log := logrus.New()
	log.Out = &buf
	l := &lineLogger{log: log}
	// Partial writes must be joined into lines.
	_, _ = l.Write([]byte("[RUNNER 2026-10-26 16:29:24Z INFO Terminal] WRITE LINE: x\n√ Connec"))
	_, _ = l.Write([]byte("ted to GitHub\n\n"))
	if out := buf.String(); strings.Contains(out, "[RUNNER") || !strings.Contains(out, `msg="√ Connected to GitHub"`) {
		t.Fatalf("got:\n%s", out)
	}
}
