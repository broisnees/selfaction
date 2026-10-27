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
	Mode          string   // "host" runs jobs on this machine, "docker" in a fresh container per job
	GroupID       int64    // runner group (1 = Default)
	Dir           string   // working directory for runner installs and workspaces
	RunnerVersion string   // actions/runner version for host mode; "" = latest
	Image         string   // runner image for docker mode
	DockerSocket  bool     // docker mode: give jobs the host Docker daemon (container jobs, services, docker build)
	ShareHome     bool     // host mode: run jobs with this user's HOME instead of a per-slot one
}

type Agent struct {
	Config
	GitHub *GitHub
	Log    logrus.FieldLogger

	labels      []string
	version     string
	healthEvery time.Duration // how often to ask GitHub whether a runner is online; 0 = 2m
}

var errOffline = errors.New("GitHub reported the runner offline; restarted it")

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
	case "docker":
		a.Log.Infof("pulling %s", a.Image)
		pull := exec.CommandContext(ctx, "docker", "pull", "--quiet", a.Image)
		if out, err := pull.CombinedOutput(); err != nil {
			return fmt.Errorf("docker pull %s: %v: %s", a.Image, err, out)
		}
	default:
		return fmt.Errorf("unknown mode %q (want host or docker)", a.Mode)
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

	jobCtx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	var cmd *exec.Cmd
	env := os.Environ()
	if a.Mode == "docker" {
		args := []string{"run", "--rm", "--name", name, "--label", "dev.selfaction.agent=1", "-e", "ACTIONS_RUNNER_INPUT_JITCONFIG", "-e", "RUNNER_MANUALLY_TRAP_SIG"}
		if a.DockerSocket {
			args = append(args, "-v", "/var/run/docker.sock:/var/run/docker.sock")
		}
		cmd = exec.CommandContext(jobCtx, "docker", append(args, a.Image, "/home/runner/run.sh")...)
	} else {
		dir := a.slotDir(n)
		defer cleanSlot(dir, false)
		cmd = exec.CommandContext(jobCtx, filepath.Join(dir, "run.sh"))
		cmd.Dir = dir
		env = a.hostEnv(env, dir)
	}
	// The runner reads ACTIONS_RUNNER_INPUT_* and scrubs it from its environment,
	// which keeps the credential out of `ps` output, unlike --jitconfig.
	// RUNNER_MANUALLY_TRAP_SIG makes run.sh forward SIGINT to the listener; by
	// default bash sits in `wait` and the listener never sees it (and as PID 1
	// in a container, run.sh would ignore it outright).
	cmd.Env = append(env, "ACTIONS_RUNNER_INPUT_JITCONFIG="+jit.Config, "RUNNER_MANUALLY_TRAP_SIG=1")
	out := &lineLogger{log: a.Log.WithField("runner", name)}
	cmd.Stdout, cmd.Stderr = out, out
	// On shutdown let the runner cancel its job and report back before killing it.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 45 * time.Second

	a.Log.Infof("%s: online, waiting for a job", name)
	go a.watch(jobCtx, stop, name, jit.ID)
	err = cmd.Run()
	switch {
	case ctx.Err() != nil:
		return nil
	case errors.Is(context.Cause(jobCtx), errOffline):
		return fmt.Errorf("%s: %w", name, errOffline)
	case err != nil:
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// watch stops a runner that GitHub has seen offline on two checks in a row,
// so the slot registers a fresh one. That happens when the machine sleeps:
// the listener's long poll goes stale but the process never exits.
func (a *Agent) watch(ctx context.Context, stop context.CancelCauseFunc, name string, id int64) {
	every := a.healthEvery
	if every == 0 {
		every = 2 * time.Minute
	}
	t := time.NewTicker(every)
	defer t.Stop()
	offline := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		status, err := a.GitHub.RunnerStatus(ctx, a.Target, id)
		switch {
		case err != nil, status == "gone":
			// An API error says nothing about the runner, and neither does a 404:
			// a renamed owner or a permissions change produces one too, and GitHub
			// disconnects runners whose registration really was deleted.
		case status == "online":
			offline = 0
		default:
			if offline++; offline >= 2 {
				a.Log.Warnf("%s: GitHub reports it %s; restarting it", name, status)
				stop(errOffline)
				return
			}
		}
	}
}

// hostEnv gives host-mode jobs a persistent per-slot HOME. Without it a job
// runs as you: actions/setup-go caches your whole ~/go/pkg/mod, npm and pip use
// your caches, and your dotfiles and SSH keys are one `cat` away. Docker keeps
// working because DOCKER_CONFIG still points at your contexts and logins.
func (a *Agent) hostEnv(env []string, dir string) []string {
	if a.ShareHome {
		return env
	}
	home := filepath.Join(dir, "_home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		a.Log.Warnf("slot home %s: %v; jobs will use your HOME", home, err)
		return env
	}
	if real, err := os.UserHomeDir(); err == nil && os.Getenv("DOCKER_CONFIG") == "" {
		env = append(env, "DOCKER_CONFIG="+filepath.Join(real, ".docker"))
	}
	return append(env, "HOME="+home) // exec uses the last value of a duplicated key
}

func (a *Agent) defaultLabels() []string {
	goos := runtime.GOOS
	if a.Mode == "docker" {
		goos = "linux"
	}
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
		// phobos: Windows host mode needs .zip extraction and run.cmd; use --mode docker there until added.
		return "", fmt.Errorf("host mode is not supported on %s/%s; use --mode docker", runtime.GOOS, runtime.GOARCH)
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
	// A previous agent that was killed outright (SIGKILL, power loss) can leave
	// listeners running in the slot directories; they would clash with new registrations.
	_ = exec.CommandContext(ctx, "pkill", "-f", filepath.Join(a.Dir, "runners")+"/").Run()
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
// actions/setup-*) and the slot's HOME (language caches) survive unless tools is set.
func cleanSlot(dir string, tools bool) {
	work := filepath.Join(dir, "_work")
	entries, _ := os.ReadDir(work)
	for _, e := range entries {
		if tools || e.Name() != "_tool" {
			os.RemoveAll(filepath.Join(work, e.Name()))
		}
	}
	if tools {
		os.RemoveAll(filepath.Join(dir, "_home"))
	}
	os.RemoveAll(filepath.Join(dir, "_diag"))
	for _, f := range []string{".runner", ".credentials", ".credentials_rsaparams"} {
		os.Remove(filepath.Join(dir, f))
	}
}

// Clean frees disk used by the agent in dir. Run it while the agent is stopped;
// a running agent already cleans each slot after every job.
func Clean(dir string, tools, runners bool) error {
	if runners {
		return errors.Join(os.RemoveAll(filepath.Join(dir, "runners")), os.RemoveAll(filepath.Join(dir, "downloads")))
	}
	slots, err := filepath.Glob(filepath.Join(dir, "runners", "*", "slot-*"))
	for _, s := range slots {
		cleanSlot(s, tools)
	}
	return err
}

// PruneDocker removes stopped containers, unused networks, dangling images and
// build cache older than age on the whole Docker host.
func PruneDocker(ctx context.Context, age time.Duration) ([]byte, error) {
	return exec.CommandContext(ctx, "docker", "system", "prune", "--force", "--filter", "until="+age.String()).CombinedOutput()
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
		switch line := strings.TrimSpace(string(l.buf[:i])); {
		case line == "":
		case strings.HasPrefix(line, "[RUNNER ") || strings.HasPrefix(line, "[WORKER "):
			// The runner image mirrors its diagnostic log to stdout; keep it for -v.
			l.log.Debug(line)
		default:
			l.log.Info(line)
		}
		l.buf = l.buf[i+1:]
	}
}
