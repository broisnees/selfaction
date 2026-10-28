// Package service installs a selfaction command as a per-user background
// service: a launchd agent on macOS, a systemd --user unit on Linux, and a
// scheduled task that starts at logon on Windows.
package service

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
	"unicode/utf16"
)

// Service is one installable command, such as `selfaction agent start …`.
type Service struct {
	Name    string   // "agent" or "worker"
	Args    []string // arguments after the selfaction executable
	LogFile string   // where its output goes
	// StopFile is a file the command watches (its --stop-file); Windows can't
	// send a scheduled task Ctrl-C, so uninstalling creates it to stop the
	// command gracefully before ending the task.
	StopFile string
}

func (s Service) label() string { return "dev.selfaction." + s.Name } // launchd label
func (s Service) unit() string  { return "selfaction-" + s.Name }     // systemd unit and Windows task name

// Install writes the service definition, (re)starts it, and returns its path.
func (s Service) Install() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return "", err
	}
	argv := append([]string{exe}, s.Args...)
	path, err := s.path()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "darwin":
		if err := os.WriteFile(path, launchdPlist(s.label(), argv, s.LogFile), 0o600); err != nil {
			return "", err
		}
		domain := fmt.Sprintf("gui/%d", os.Getuid())
		target := domain + "/" + s.label()
		_ = exec.Command("launchctl", "bootout", target).Run()
		// bootout returns while a running agent is still letting its runners cancel
		// their jobs; bootstrapping before it is gone fails with "Input/output error".
		for deadline := time.Now().Add(90 * time.Second); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
			if exec.Command("launchctl", "print", target).Run() != nil {
				break
			}
		}
		return path, run("launchctl", "bootstrap", domain, path)
	case "windows":
		s.stopTask()
		if err := os.WriteFile(path, utf16LE(taskXML(s.Name, argv)), 0o600); err != nil {
			return "", err
		}
		if err := run("schtasks", "/Create", "/TN", s.unit(), "/XML", path, "/F"); err != nil {
			return "", err
		}
		return path, run("schtasks", "/Run", "/TN", s.unit())
	}
	if err := os.WriteFile(path, systemdUnit(s.Name, argv), 0o600); err != nil {
		return "", err
	}
	if err := run("systemctl", "--user", "daemon-reload"); err != nil {
		return "", err
	}
	return path, run("systemctl", "--user", "enable", "--now", s.unit())
}

// Uninstall stops the service and removes its definition.
func (s Service) Uninstall() error {
	path, err := s.path()
	if err != nil {
		return err
	}
	switch runtime.GOOS {
	case "darwin":
		_ = exec.Command("launchctl", "bootout", fmt.Sprintf("gui/%d/%s", os.Getuid(), s.label())).Run()
	case "windows":
		s.stopTask()
		_ = exec.Command("schtasks", "/Delete", "/TN", s.unit(), "/F").Run()
	default:
		_ = exec.Command("systemctl", "--user", "disable", "--now", s.unit()).Run()
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// stopTask asks a running Windows task to stop through its stop file, waits
// for it to finish its jobs, then ends whatever is left.
func (s Service) stopTask() {
	if !taskRunning(s.unit()) {
		return
	}
	if s.StopFile != "" && os.WriteFile(s.StopFile, nil, 0o600) == nil {
		for deadline := time.Now().Add(90 * time.Second); time.Now().Before(deadline) && taskRunning(s.unit()); {
			time.Sleep(time.Second)
		}
	}
	_ = exec.Command("schtasks", "/End", "/TN", s.unit()).Run()
	if s.StopFile != "" {
		os.Remove(s.StopFile)
	}
}

func taskRunning(name string) bool {
	out, err := exec.Command("schtasks", "/Query", "/TN", name, "/FO", "CSV", "/NH").Output()
	return err == nil && bytes.Contains(out, []byte(`"Running"`))
}

func (s Service) path() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "LaunchAgents", s.label()+".plist"), nil
	case "linux":
		return filepath.Join(home, ".config", "systemd", "user", s.unit()+".service"), nil
	case "windows":
		return filepath.Join(home, ".selfaction", s.unit()+".task.xml"), nil
	}
	return "", fmt.Errorf("service install is not supported on %s; run selfaction under your own supervisor", runtime.GOOS)
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func launchdPlist(label string, argv []string, logFile string) []byte {
	var args strings.Builder
	for _, a := range argv {
		args.WriteString("\n\t\t<string>" + xmlEscape(a) + "</string>")
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
`, label, args.String(), xmlEscape(os.Getenv("PATH")), xmlEscape(logFile), xmlEscape(logFile))
}

func systemdUnit(name string, argv []string) []byte {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = strings.ReplaceAll(strconv.Quote(a), "%", "%%")
	}
	return fmt.Appendf(nil, `[Unit]
Description=selfaction %s
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
`, name, strings.Join(q, " "), strings.ReplaceAll(strconv.Quote("PATH="+os.Getenv("PATH")), "%", "%%"))
}

// windowsArgs joins arguments the way Windows programs (and Go's os.Args) split them.
func windowsArgs(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = windowsQuote(a)
	}
	return strings.Join(q, " ")
}

func windowsQuote(a string) string {
	if a != "" && !strings.ContainsAny(a, " \t\"") {
		return a
	}
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for _, r := range a {
		switch r {
		case '\\':
			slashes++
		case '"':
			b.WriteString(strings.Repeat(`\`, 2*slashes+1))
			b.WriteRune(r)
			slashes = 0
			continue
		default:
			b.WriteString(strings.Repeat(`\`, slashes))
			slashes = 0
			b.WriteRune(r)
			continue
		}
	}
	b.WriteString(strings.Repeat(`\`, 2*slashes))
	b.WriteByte('"')
	return b.String()
}

// taskXML is a Task Scheduler definition that starts argv when the current
// user logs on, with no time limit, restarting it if it exits with an error.
func taskXML(name string, argv []string) []byte {
	user := os.Getenv("USERDOMAIN") + `\` + os.Getenv("USERNAME")
	if os.Getenv("USERDOMAIN") == "" {
		user = os.Getenv("USERNAME")
	}
	return fmt.Appendf(nil, `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo><Description>selfaction %s</Description></RegistrationInfo>
  <Triggers><LogonTrigger><Enabled>true</Enabled><UserId>%s</UserId></LogonTrigger></Triggers>
  <Principals><Principal id="Author"><UserId>%s</UserId><LogonType>InteractiveToken</LogonType><RunLevel>LeastPrivilege</RunLevel></Principal></Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <RestartOnFailure><Interval>PT1M</Interval><Count>999</Count></RestartOnFailure>
    <Hidden>true</Hidden>
  </Settings>
  <Actions Context="Author"><Exec><Command>%s</Command><Arguments>%s</Arguments></Exec></Actions>
</Task>
`, xmlEscape(name), xmlEscape(user), xmlEscape(user), xmlEscape(argv[0]), xmlEscape(windowsArgs(argv[1:])))
}

// utf16LE encodes text as UTF-16 with a byte order mark, which schtasks expects.
func utf16LE(b []byte) []byte {
	out := []byte{0xFF, 0xFE}
	for _, u := range utf16.Encode([]rune(string(b))) {
		out = append(out, byte(u), byte(u>>8))
	}
	return out
}

func run(name string, args ...string) error {
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return nil
}
