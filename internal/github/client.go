// Package github is the small GitHub REST client shared by the agent and the server.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Client talks to https://api.github.com, or a GitHub Enterprise Server base
// such as https://ghe.example.com/api/v3.
type Client struct {
	API   string
	Token string
	HTTP  *http.Client
}

// StatusError is a non-2xx response.
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string { return fmt.Sprintf("github api: %d %s", e.Code, e.Body) }

func IsNotFound(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code == http.StatusNotFound
}

// Do sends in as JSON and decodes the response into out (either may be nil).
// path is relative to API ("/repos/…") or an absolute URL.
func (c *Client) Do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	res, err := c.send(ctx, method, path, body, "application/vnd.github+json")
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if out == nil {
		return nil
	}
	return json.NewDecoder(res.Body).Decode(out)
}

// Raw returns a response body as is, at most limit bytes; for file contents
// use the "application/vnd.github.raw+json" media type.
func (c *Client) Raw(ctx context.Context, path, accept string, limit int64) ([]byte, error) {
	res, err := c.send(ctx, "GET", path, nil, accept)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err == nil && int64(len(b)) > limit {
		err = fmt.Errorf("%s: response larger than %d bytes", path, limit)
	}
	return b, err
}

func (c *Client) send(ctx context.Context, method, path string, body io.Reader, accept string) (*http.Response, error) {
	url := path
	if !strings.HasPrefix(path, "http://") && !strings.HasPrefix(path, "https://") {
		url = strings.TrimSuffix(c.API, "/") + path
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 300 {
		defer res.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return nil, &StatusError{Code: res.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	return res, nil
}
