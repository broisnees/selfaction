package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

// TestProtocol drives the server exactly like actions/cache does: reserve,
// concurrent out-of-order chunk upload, commit, lookup, download.
func TestProtocol(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	srv := httptest.NewServer((&Server{Store: store, Token: "tok", Log: logrus.New()}).Handler())
	defer srv.Close()
	api := srv.URL + "/tok/me.repo/_apis/artifactcache"

	call := func(method, url string, body []byte, hdr ...string) (int, []byte) {
		req, _ := http.NewRequest(method, url, bytes.NewReader(body))
		for i := 0; i < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, b
	}
	save := func(key, data string) {
		code, body := call("POST", api+"/caches", []byte(fmt.Sprintf(`{"key":%q,"version":"v1","cacheSize":%d}`, key, len(data))))
		if code != 200 {
			t.Fatalf("reserve %s: %d %s", key, code, body)
		}
		var res struct{ CacheID int64 }
		_ = json.Unmarshal(body, &res)
		half := len(data) / 2
		// second half first: chunks are written at their offsets, order must not matter
		for _, c := range [][2]int{{half, len(data)}, {0, half}} {
			rng := fmt.Sprintf("bytes %d-%d/*", c[0], c[1]-1)
			if code, body := call("PATCH", fmt.Sprintf("%s/caches/%d", api, res.CacheID), []byte(data[c[0]:c[1]]), "Content-Range", rng); code != 200 {
				t.Fatalf("upload: %d %s", code, body)
			}
		}
		if code, body := call("POST", fmt.Sprintf("%s/caches/%d", api, res.CacheID), []byte(fmt.Sprintf(`{"size":%d}`, len(data)))); code != 200 {
			t.Fatalf("commit: %d %s", code, body)
		}
	}
	restore := func(keys string) (string, string) {
		code, body := call("GET", api+"/cache?version=v1&keys="+keys, nil)
		if code == 204 {
			return "", ""
		}
		var hit struct{ CacheKey, ArchiveLocation string }
		if err := json.Unmarshal(body, &hit); err != nil || code != 200 {
			t.Fatalf("find %s: %d %s", keys, code, body)
		}
		_, data := call("GET", hit.ArchiveLocation, nil)
		return hit.CacheKey, string(data)
	}

	save("npm-linux-aaa", "first archive")
	time.Sleep(1100 * time.Millisecond) // created_at has 1s resolution; make "newest" unambiguous
	save("npm-linux-bbb", "second archive!")

	cases := []struct{ keys, wantKey, wantData string }{
		{"npm-linux-aaa", "npm-linux-aaa", "first archive"},              // exact
		{"npm-linux-zzz,npm-linux-", "npm-linux-bbb", "second archive!"}, // restore-key prefix → newest
		{"npm-linux-,npm-linux-aaa", "npm-linux-bbb", "second archive!"}, // earlier key wins over later exact
		{"npm-linux-aa", "npm-linux-aaa", "first archive"},               // prefix
		{"pip-", "", ""}, // miss
	}
	for _, c := range cases {
		if k, d := restore(c.keys); k != c.wantKey || d != c.wantData {
			t.Errorf("restore(%s) = %q %q, want %q %q", c.keys, k, d, c.wantKey, c.wantData)
		}
	}

	if code, _ := call("POST", api+"/caches", []byte(`{"key":"npm-linux-aaa","version":"v1"}`)); code != 409 {
		t.Errorf("re-reserving a committed key: got %d, want 409", code)
	}
	if code, _ := call("GET", srv.URL+"/wrong/me.repo/_apis/artifactcache/cache?keys=npm&version=v1", nil); code != 401 {
		t.Errorf("bad token: got %d, want 401", code)
	}
	if code, _ := call("GET", srv.URL+"/tok/other.repo/_apis/artifactcache/cache?keys=npm&version=v1", nil); code != 204 {
		t.Errorf("other scope must not see me.repo caches: got %d", code)
	}

	removed, err := store.Prune(context.Background(), Filter{Scope: "me.repo", KeyPrefix: "npm-linux-a"})
	if err != nil || len(removed) != 1 || removed[0].Key != "npm-linux-aaa" {
		t.Fatalf("prune: %v %+v", err, removed)
	}
	if k, _ := restore("npm-linux-aaa"); k != "" {
		t.Errorf("pruned key still restores as %q", k)
	}
	if k, _ := restore("npm-linux-aaa,npm-linux-"); k != "npm-linux-bbb" {
		t.Errorf("after prune, restore key should match the survivor, got %q", k)
	}
}

func TestGCEvictsLeastRecentlyUsed(t *testing.T) {
	ctx := context.Background()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	put := func(scope, key string, size int, usedAgo time.Duration) int64 {
		id, err := store.Reserve(ctx, scope, key, "v")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Write(ctx, scope, id, 0, bytes.NewReader(make([]byte, size))); err != nil {
			t.Fatal(err)
		}
		if err := store.Commit(ctx, scope, id, int64(size)); err != nil {
			t.Fatal(err)
		}
		_, _ = store.db.Exec(`UPDATE caches SET used_at = ? WHERE id = ?`, time.Now().Add(-usedAgo).Unix(), id)
		return id
	}
	put("a", "old", 100, 3*time.Hour)
	put("a", "mid", 100, 2*time.Hour)
	put("a", "new", 100, time.Hour)
	put("b", "stale", 10, 10*24*time.Hour)
	put("b", "fresh", 10, 0)
	pending, _ := store.Reserve(ctx, "b", "abandoned", "v")
	_, _ = store.db.Exec(`UPDATE caches SET used_at = ? WHERE id = ?`, time.Now().Add(-time.Hour).Unix(), pending)

	n, freed, err := store.GC(ctx, Policy{TTL: 7 * 24 * time.Hour, MaxScopeBytes: 250})
	if err != nil {
		t.Fatal(err)
	}
	left, _ := store.List(ctx, Filter{})
	var keys []string
	for _, e := range left {
		keys = append(keys, e.Scope+"/"+e.Key)
	}
	// stale (TTL) + abandoned (stale upload) + old (scope a over 250 bytes)
	if n != 3 || freed != 110 || fmt.Sprint(keys) != "[a/new a/mid b/fresh]" {
		t.Fatalf("gc removed %d (%d bytes), left %v", n, freed, keys)
	}
}

func TestScopeOf(t *testing.T) {
	for in, want := range map[string]string{"octo/hello-world": "octo.hello-world", "a b/c": "a-b.c", "already.ok": "already.ok"} {
		if got := ScopeOf(in); got != want || !ValidScope(got) {
			t.Errorf("ScopeOf(%q) = %q, want %q", in, got, want)
		}
	}
}
