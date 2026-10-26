// Package agent turns any machine into a pool of GitHub Actions runners.
// Each slot registers a just-in-time (single-job) runner with GitHub, runs the
// official actions/runner until that job finishes, cleans up, and repeats, so
// workflows target it with `runs-on: [self-hosted, <label>]` and run with full
// GitHub compatibility.
package agent

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"

	"github.com/sirupsen/logrus"
)

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
