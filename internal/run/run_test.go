package run

import (
	"bytes"
	"testing"

	log "github.com/sirupsen/logrus"
)

func TestPlainLogs(t *testing.T) {
	var buf bytes.Buffer
	l := plainLogs{&buf}.WithJobLogger()
	l.WithField("job", "ci/test").Info("⭐ Run Main go test")
	l.WithFields(log.Fields{"job": "ci/test", "raw_output": true}).Info("ok  example.com/x\n")
	want := "[ci/test] ⭐ Run Main go test\n[ci/test]   | ok  example.com/x\n"
	if buf.String() != want {
		t.Errorf("got %q, want %q", buf.String(), want)
	}
}
