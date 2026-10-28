package service

import (
	"strings"
	"testing"
)

func TestWindowsQuoting(t *testing.T) {
	cases := map[string]string{
		`plain`:                `plain`,
		`C:\Program Files\x`:   `"C:\Program Files\x"`,
		`say "hi"`:             `"say \"hi\""`,
		`trailing\ slash\`:     `"trailing\ slash\\"`,
		``:                     `""`,
		`--home=C:\Users\a b\`: `"--home=C:\Users\a b\\"`,
	}
	for in, want := range cases {
		if got := windowsQuote(in); got != want {
			t.Errorf("windowsQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestDefinitions(t *testing.T) {
	argv := []string{`/usr/local/bin/selfaction`, "worker", "--server=http://ci:8080", "--labels=a&b"}
	if p := string(launchdPlist("dev.selfaction.worker", argv, "/tmp/w.log")); !strings.Contains(p, "<string>--labels=a&amp;b</string>") ||
		!strings.Contains(p, "<key>Label</key><string>dev.selfaction.worker</string>") {
		t.Errorf("plist:\n%s", p)
	}
	if u := string(systemdUnit("worker", argv)); !strings.Contains(u, `ExecStart="/usr/local/bin/selfaction" "worker"`) {
		t.Errorf("unit:\n%s", u)
	}
	x := string(taskXML("worker", []string{`C:\bin\selfaction.exe`, "worker", "--home=C:\\Users\\me\\.selfaction"}))
	for _, want := range []string{"<Command>C:\\bin\\selfaction.exe</Command>", "<Arguments>worker --home=C:\\Users\\me\\.selfaction</Arguments>",
		"<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>", "<LogonTrigger>"} {
		if !strings.Contains(x, want) {
			t.Errorf("task XML lacks %s:\n%s", want, x)
		}
	}
}
