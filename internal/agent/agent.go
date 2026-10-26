// Package agent turns any machine into a pool of GitHub Actions runners.
// Each slot registers a just-in-time (single-job) runner with GitHub, runs the
// official actions/runner until that job finishes, cleans up, and repeats, so
// workflows target it with `runs-on: [self-hosted, <label>]` and run with full
// GitHub compatibility.
package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

type Config struct {
	Target        string   // "owner/repo" or an organization
	Name          string   // runner name prefix
	Labels        []string // extra labels on top of self-hosted, OS and arch
	Slots         int      // concurrent jobs
	Mode          string   // "host" runs jobs on this machine
	GroupID       int64    // runner group (1 = Default)
	Dir           string   // working directory for runner installs and workspaces
	RunnerVersion string   // actions/runner version for host mode; "" = latest
}

type Agent struct {
	Config
	GitHub *GitHub
	Log    logrus.FieldLogger

	labels  []string
	version string
}

// Run keeps Slots runners registered until ctx is cancelled, then
// unregisters them.
func (a *Agent) Run(ctx context.Context) error {
	if a.Slots < 1 {
		a.Slots = 1
	}
	a.labels = a.defaultLabels()
	switch a.Mode {
	case "host":
		if err := a.prepareHost(ctx); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown mode %q (want host)", a.Mode)
	}
	a.Log.Infof("serving %s with %d slot(s), labels %s", a.Target, a.Slots, strings.Join(a.labels, ","))

	var wg sync.WaitGroup
	for n := 1; n <= a.Slots; n++ {
		wg.Go(func() { a.slot(ctx, n) })
	}
	wg.Wait()
	return nil
}

func (a *Agent) slot(ctx context.Context, n int) {
	backoff := 5 * time.Second
	for ctx.Err() == nil {
		start := time.Now()
		if err := a.runJob(ctx, n); err != nil {
			a.Log.Errorf("slot %d: %v", n, err)
		}
		if time.Since(start) > 30*time.Second {
			backoff = 5 * time.Second
			continue
		}
		// The runner died right away (bad token, API outage, wrong labels): don't hammer GitHub.
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 5*time.Minute)
	}
}

// runJob registers one JIT runner and blocks until it has run a job (or ctx ends).
func (a *Agent) runJob(ctx context.Context, n int) error {
	name := fmt.Sprintf("%s-%d-%s", a.Name, n, randHex(3))
	jit, err := a.GitHub.CreateJITRunner(ctx, a.Target, name, a.GroupID, a.labels)
	if err != nil {
		return err
	}
	defer func() {
		// Ephemeral runners remove themselves after a job; this covers crashes and shutdown.
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		if err := a.GitHub.DeleteRunner(dctx, a.Target, jit.ID); err != nil {
			a.Log.Warnf("%s: unregister: %v", name, err)
		}
	}()

	dir := a.slotDir(n)
	defer cleanSlot(dir, false)
	cmd := exec.CommandContext(ctx, filepath.Join(dir, "run.sh"))
	cmd.Dir = dir
	// The runner reads ACTIONS_RUNNER_INPUT_* and scrubs it from its environment,
	// which keeps the credential out of `ps` output, unlike --jitconfig.
	cmd.Env = append(os.Environ(), "ACTIONS_RUNNER_INPUT_JITCONFIG="+jit.Config)
	out := &lineLogger{log: a.Log.WithField("runner", name)}
	cmd.Stdout, cmd.Stderr = out, out
	// On shutdown let the runner cancel its job and report back before killing it.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 30 * time.Second

	a.Log.Infof("%s: online, waiting for a job", name)
	if err := cmd.Run(); err != nil && ctx.Err() == nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

func (a *Agent) defaultLabels() []string {
	goos := runtime.GOOS
	base := []string{
		"self-hosted",
		map[string]string{"darwin": "macOS", "linux": "Linux", "windows": "Windows"}[goos],
		map[string]string{"amd64": "X64", "arm64": "ARM64", "arm": "ARM"}[runtime.GOARCH],
	}
	seen := map[string]bool{}
	var out []string
	for _, l := range append(base, a.Labels...) {
		if k := strings.ToLower(strings.TrimSpace(l)); k != "" && !seen[k] {
			seen[k] = true
			out = append(out, strings.TrimSpace(l))
		}
	}
	return out
}

// runnerPlatform is the actions/runner release name for this machine, e.g. osx-arm64.
func runnerPlatform() (string, error) {
	osName, ok := map[string]string{"darwin": "osx", "linux": "linux"}[runtime.GOOS]
	arch, ok2 := map[string]string{"amd64": "x64", "arm64": "arm64", "arm": "arm"}[runtime.GOARCH]
	if !ok || !ok2 {
		// phobos: Windows host mode needs .zip extraction and run.cmd; add when Windows hosts matter.
		return "", fmt.Errorf("host mode is not supported on %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	return osName + "-" + arch, nil
}

func (a *Agent) slotDir(n int) string {
	return filepath.Join(a.Dir, "runners", a.version, fmt.Sprintf("slot-%d", n))
}

// prepareHost downloads and verifies the runner release once, unpacks one copy
// per slot (each JIT runner writes its credentials into its own directory), and
// deletes installs of older versions.
func (a *Agent) prepareHost(ctx context.Context) error {
	platform, err := runnerPlatform()
	if err != nil {
		return err
	}
	rel, err := RunnerRelease(ctx, http.DefaultClient, a.RunnerVersion)
	if err != nil {
		return err
	}
	a.version = rel.Version
	file := fmt.Sprintf("actions-runner-%s-%s.tar.gz", platform, rel.Version)
	tarball := filepath.Join(a.Dir, "downloads", file)
	if _, err := os.Stat(tarball); err != nil {
		a.Log.Infof("downloading actions/runner %s (%s)", rel.Version, platform)
		url := fmt.Sprintf("https://github.com/actions/runner/releases/download/v%s/%s", rel.Version, file)
		if err := download(ctx, url, tarball, rel.SHA256[platform]); err != nil {
			return err
		}
	}
	for n := 1; n <= a.Slots; n++ {
		dir := a.slotDir(n)
		if _, err := os.Stat(dir); err == nil {
			continue
		}
		if err := extract(tarball, dir); err != nil {
			return fmt.Errorf("unpack runner for slot %d: %w", n, err)
		}
	}
	removeExcept(filepath.Join(a.Dir, "runners"), rel.Version)
	removeExcept(filepath.Join(a.Dir, "downloads"), file)
	return nil
}

func download(ctx context.Context, url, dest, wantSHA string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("download %s: %s", url, res.Status)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), res.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && wantSHA != "" && hex.EncodeToString(h.Sum(nil)) != wantSHA {
		err = fmt.Errorf("checksum mismatch for %s", url)
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if wantSHA == "" {
		logrus.Warnf("no published checksum for %s; trusting HTTPS only", filepath.Base(dest))
	}
	return os.Rename(tmp, dest)
}

// extract unpacks a .tar.gz into dest atomically (via a temp dir) and refuses
// entries that would land outside it.
func extract(tarball, dest string) error {
	f, err := os.Open(tarball)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tmp := dest + ".tmp"
	os.RemoveAll(tmp)
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(hdr.Name)
		if !filepath.IsLocal(name) {
			return fmt.Errorf("unsafe path in archive: %q", hdr.Name)
		}
		target := filepath.Join(tmp, name)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			err = os.MkdirAll(target, 0o755)
		case tar.TypeSymlink:
			err = os.Symlink(hdr.Linkname, target)
		case tar.TypeReg:
			var out *os.File
			if out, err = os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, hdr.FileInfo().Mode().Perm()); err == nil {
				_, err = io.Copy(out, tr)
				if cerr := out.Close(); err == nil {
					err = cerr
				}
			}
		}
		if err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}

func removeExcept(dir, keep string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != keep {
			os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
}

// cleanSlot wipes a slot after a job: workspace, diagnostics and the JIT
// credentials the runner wrote. The tool cache (_work/_tool, used by
// actions/setup-*) survives unless tools is set.
func cleanSlot(dir string, tools bool) {
	work := filepath.Join(dir, "_work")
	entries, _ := os.ReadDir(work)
	for _, e := range entries {
		if tools || e.Name() != "_tool" {
			os.RemoveAll(filepath.Join(work, e.Name()))
		}
	}
	os.RemoveAll(filepath.Join(dir, "_diag"))
	for _, f := range []string{".runner", ".credentials", ".credentials_rsaparams"} {
		os.Remove(filepath.Join(dir, f))
	}
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// lineLogger forwards a process's output to the logger line by line.
type lineLogger struct {
	log logrus.FieldLogger
	buf []byte
}

func (l *lineLogger) Write(p []byte) (int, error) {
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			return len(p), nil
		}
		if line := strings.TrimSpace(string(l.buf[:i])); line != "" {
			l.log.Info(line)
		}
		l.buf = l.buf[i+1:]
	}
}
