package agent

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const serviceLabel = "dev.selfaction.agent"

// Install registers `selfaction agent start <args>` as a per-user background
// service (launchd on macOS, systemd --user on Linux) and starts it.
func Install(args []string, logFile string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return "", err
	}
	argv := append([]string{exe, "agent", "start"}, args...)
	path, err := servicePath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if runtime.GOOS == "darwin" {
		if err := os.WriteFile(path, launchdPlist(argv, logFile), 0o600); err != nil {
			return "", err
		}
		domain := fmt.Sprintf("gui/%d", os.Getuid())
		target := domain + "/" + serviceLabel
		_ = exec.Command("launchctl", "bootout", target).Run()
		// bootout returns while a running agent is still letting its runners cancel
		// their jobs; bootstrapping before it is gone fails with "Input/output error".
		for deadline := time.Now().Add(90 * time.Second); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
			if exec.Command("launchctl", "print", target).Run() != nil {
				break
			}
		}
		return path, run("launchctl", "bootstrap", domain, path)
	}
	if err := os.WriteFile(path, systemdUnit(argv), 0o600); err != nil {
		return "", err
	}
	if err := run("systemctl", "--user", "daemon-reload"); err != nil {
		return "", err
	}
	return path, run("systemctl", "--user", "enable", "--now", "selfaction-agent")
}

func Uninstall() error {
	path, err := servicePath()
	if err != nil {
		return err
	}
	if runtime.GOOS == "darwin" {
		_ = exec.Command("launchctl", "bootout", fmt.Sprintf("gui/%d/%s", os.Getuid(), serviceLabel)).Run()
	} else {
		_ = exec.Command("systemctl", "--user", "disable", "--now", "selfaction-agent").Run()
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func servicePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "LaunchAgents", serviceLabel+".plist"), nil
	case "linux":
		return filepath.Join(home, ".config", "systemd", "user", "selfaction-agent.service"), nil
	}
	return "", fmt.Errorf("service install is not supported on %s; run `selfaction agent start` under your own supervisor", runtime.GOOS)
}

func launchdPlist(argv []string, logFile string) []byte {
	esc := func(s string) string {
		var b bytes.Buffer
		_ = xml.EscapeText(&b, []byte(s))
		return b.String()
	}
	var args strings.Builder
	for _, a := range argv {
		args.WriteString("\n\t\t<string>" + esc(a) + "</string>")
	}
	return fmt.Appendf(nil, `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>%s</string>
	<key>ProgramArguments</key>
	<array>%s
	</array>
	<key>EnvironmentVariables</key>
	<dict><key>PATH</key><string>%s</string></dict>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><true/>
	<key>StandardOutPath</key><string>%s</string>
	<key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, serviceLabel, args.String(), esc(os.Getenv("PATH")), esc(logFile), esc(logFile))
}

func systemdUnit(argv []string) []byte {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = strings.ReplaceAll(strconv.Quote(a), "%", "%%")
	}
	return fmt.Appendf(nil, `[Unit]
Description=selfaction agent (GitHub Actions runners)
After=network-online.target

[Service]
ExecStart=%s
Environment=%s
Restart=always
RestartSec=10
KillSignal=SIGINT
TimeoutStopSec=60

[Install]
WantedBy=default.target
`, strings.Join(q, " "), strings.ReplaceAll(strconv.Quote("PATH="+os.Getenv("PATH")), "%", "%%"))
}

func run(name string, args ...string) error {
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return nil
}
