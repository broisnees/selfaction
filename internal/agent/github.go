package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// GitHub is the slice of the REST API the agent needs. API is
// https://api.github.com or a GHES base like https://ghe.example.com/api/v3.
type GitHub struct {
	API   string
	Token string
	HTTP  *http.Client
}

// JITRunner is a single-use runner registration.
type JITRunner struct {
	ID     int64
	Name   string
	Config string // encoded_jit_config, a credential: never log it
}

// targetPath maps "owner/repo" to /repos/owner/repo and "org" to /orgs/org.
func targetPath(target string) string {
	if strings.Contains(target, "/") {
		return "/repos/" + target
	}
	return "/orgs/" + target
}

func (g *GitHub) CreateJITRunner(ctx context.Context, target, name string, groupID int64, labels []string) (*JITRunner, error) {
	var res struct {
		Runner struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"runner"`
		EncodedJITConfig string `json:"encoded_jit_config"`
	}
	body := map[string]any{"name": name, "runner_group_id": groupID, "labels": labels, "work_folder": "_work"}
	if err := g.do(ctx, "POST", g.API+targetPath(target)+"/actions/runners/generate-jitconfig", body, &res); err != nil {
		return nil, fmt.Errorf("create runner %s: %w", name, err)
	}
	return &JITRunner{ID: res.Runner.ID, Name: res.Runner.Name, Config: res.EncodedJITConfig}, nil
}

// DeleteRunner removes a registration. Already-gone runners (ephemeral runners
// delete themselves after their job) are not an error.
func (g *GitHub) DeleteRunner(ctx context.Context, target string, id int64) error {
	err := g.do(ctx, "DELETE", fmt.Sprintf("%s%s/actions/runners/%d", g.API, targetPath(target), id), nil, nil)
	if se, ok := err.(*statusError); ok && se.code == http.StatusNotFound {
		return nil
	}
	return err
}

// RunnerStatus reports "online" or "offline" for a registration, or "gone"
// when GitHub no longer knows it.
func (g *GitHub) RunnerStatus(ctx context.Context, target string, id int64) (string, error) {
	var res struct {
		Status string `json:"status"`
	}
	err := g.do(ctx, "GET", fmt.Sprintf("%s%s/actions/runners/%d", g.API, targetPath(target), id), nil, &res)
	if se, ok := err.(*statusError); ok && se.code == http.StatusNotFound {
		return "gone", nil
	}
	return res.Status, err
}

// Release is an actions/runner release; SHA256 maps "osx-arm64" style
// platforms to the tarball checksum published in the release notes.
type Release struct {
	Version string
	SHA256  map[string]string
}

var shaRe = regexp.MustCompile(`<!-- BEGIN SHA ([a-z0-9_-]+) -->([0-9a-f]{64})<!-- END SHA`)

// RunnerRelease fetches the latest (version "") or a specific actions/runner
// release. Runner binaries always come from github.com, even for GHES.
func RunnerRelease(ctx context.Context, client *http.Client, version string) (*Release, error) {
	url := "https://api.github.com/repos/actions/runner/releases/latest"
	if version != "" {
		url = "https://api.github.com/repos/actions/runner/releases/tags/v" + strings.TrimPrefix(version, "v")
	}
	var res struct {
		TagName string `json:"tag_name"`
		Body    string `json:"body"`
	}
	if err := (&GitHub{HTTP: client}).do(ctx, "GET", url, nil, &res); err != nil {
		return nil, fmt.Errorf("look up actions/runner release: %w", err)
	}
	rel := &Release{Version: strings.TrimPrefix(res.TagName, "v"), SHA256: map[string]string{}}
	for _, m := range shaRe.FindAllStringSubmatch(res.Body, -1) {
		rel.SHA256[m[1]] = m[2]
	}
	return rel, nil
}

type statusError struct {
	code int
	body string
}

func (e *statusError) Error() string { return fmt.Sprintf("github api: %d %s", e.code, e.body) }

func (g *GitHub) do(ctx context.Context, method, url string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if g.Token != "" {
		req.Header.Set("Authorization", "Bearer "+g.Token)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := g.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return &statusError{code: res.StatusCode, body: strings.TrimSpace(string(b))}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(res.Body).Decode(out)
}
