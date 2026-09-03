package main

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

func cacheArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()

	var compressed bytes.Buffer
	encoder, err := zstd.NewWriter(&compressed)

	if err != nil {
		t.Fatal(err)
	}

	tw := tar.NewWriter(encoder)

	if err := tw.WriteHeader(&tar.Header{Name: ".", Typeflag: tar.TypeDir, Mode: 0755}); err != nil {
		t.Fatal(err)
	}

	for name, value := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0644, Size: int64(len(value))}); err != nil {
			t.Fatal(err)
		}

		if _, err := io.WriteString(tw, value); err != nil {
			t.Fatal(err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}

	return compressed.Bytes()
}

func TestPackageCacheResolveCyclesEndpoints(t *testing.T) {
	var failedCalls atomic.Int32
	failed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failedCalls.Add(1)
		http.Error(w, "try later", http.StatusServiceUnavailable)
	}))
	defer failed.Close()

	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/resolve" {
			http.NotFound(w, r)

			return
		}

		var uids []string

		if err := json.NewDecoder(r.Body).Decode(&uids); err != nil {
			t.Fatal(err)
		}

		if len(uids) != 2 {
			t.Fatalf("uids=%v", uids)
		}

		_ = json.NewEncoder(w).Encode([]string{"cached"})
	}))
	defer good.Close()

	var sleeps []time.Duration
	cache := &packageCache{
		endpoints: []string{failed.URL, good.URL},
		available: map[string]bool{},
		http:      &http.Client{},
		sleep: func(delay time.Duration) {
			sleeps = append(sleeps, delay)
		},
	}
	cache.available = cache.resolve([]string{"cached", "local"})

	if !cache.has("cached") || cache.has("local") {
		t.Fatalf("available=%v", cache.available)
	}

	if failedCalls.Load() != 1 {
		t.Fatalf("failed endpoint calls=%d", failedCalls.Load())
	}

	if len(sleeps) != 1 || sleeps[0] < 750*time.Millisecond || sleeps[0] >= 2250*time.Millisecond {
		t.Fatalf("retry sleeps=%v", sleeps)
	}
}

func TestPackageCacheResolveDropsPermanentEndpoint(t *testing.T) {
	var calls atomic.Int32
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer bad.Close()

	cache := &packageCache{
		endpoints: []string{bad.URL},
		http:      &http.Client{},
		sleep: func(time.Duration) {
			t.Fatal("unexpected retry sleep")
		},
	}

	if available := cache.resolve([]string{"local"}); len(available) != 0 {
		t.Fatalf("available=%v", available)
	}

	if calls.Load() != 1 {
		t.Fatalf("permanent endpoint calls=%d", calls.Load())
	}

	if len(cache.endpoints) != 1 || cache.endpoints[0] != bad.URL {
		t.Fatalf("blob endpoints=%v", cache.endpoints)
	}
}

func TestPackageCacheResolveDropDoesNotAffectBlobFetch(t *testing.T) {
	blob := cacheArchive(t, map[string]string{"value": "from resolve-rejected endpoint"})
	var blobCalls atomic.Int32
	rejected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/resolve":
			http.Error(w, "bad request", http.StatusBadRequest)
		case "/v1/blob/cached":
			blobCalls.Add(1)
			_, _ = w.Write(blob)
		default:
			http.NotFound(w, r)
		}
	}))
	defer rejected.Close()

	resolver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/resolve":
			_ = json.NewEncoder(w).Encode([]string{"cached"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer resolver.Close()

	cache := &packageCache{
		endpoints: []string{rejected.URL, resolver.URL},
		http:      &http.Client{},
		sleep:     func(time.Duration) {},
	}
	cache.available = cache.resolve([]string{"cached"})

	if !cache.has("cached") {
		t.Fatalf("available=%v", cache.available)
	}

	root := t.TempDir()
	cache.restore("cached", filepath.Join(root, "out"), filepath.Join(root, "trash"))

	if blobCalls.Load() != 1 {
		t.Fatalf("blob calls=%d", blobCalls.Load())
	}
}

func TestPackageCacheResolveExhaustionBuildsLocally(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer bad.Close()

	root := t.TempDir()
	target := filepath.Join(root, "target")
	t.Setenv("IX_PACKAGE_CACHE", bad.URL)

	graph := &Graph{
		Nodes: []Node{{
			UID:     "local-uid",
			OutDirs: []string{target},
			Pool:    "threads",
		}},
		Targets:  []string{target + "/touch"},
		Pools:    map[string]int{"threads": 1, "network": 1},
		TrashDir: filepath.Join(root, "trash"),
	}

	if graph.execute() {
		t.Fatal("local fallback failed")
	}

	if _, err := os.Stat(target + "/touch"); err != nil {
		t.Fatalf("local output: %v", err)
	}
}

func TestPackageCacheResolveDeadlineSequence(t *testing.T) {
	timeouts := &retryTimeouts{current: 10 * time.Second}
	first := timeouts.next()

	if first < 5*time.Second || first >= 15*time.Second {
		t.Fatalf("first deadline=%v", first)
	}

	if timeouts.current != 15*time.Second {
		t.Fatalf("second deadline base=%v", timeouts.current)
	}

	timeouts.current = 1000 * time.Second
	_ = timeouts.next()

	if timeouts.current != 1000*time.Second {
		t.Fatalf("capped deadline base=%v", timeouts.current)
	}
}

func TestPackageCacheInfraStatus(t *testing.T) {
	for _, code := range []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		if !cacheInfraStatus(code) {
			t.Fatalf("HTTP %d must be infrastructure failure", code)
		}
	}

	for _, code := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound} {
		if cacheInfraStatus(code) {
			t.Fatalf("HTTP %d must be permanent failure", code)
		}
	}
}

func TestPackageCacheRestoreDropsNotFoundEndpoint(t *testing.T) {
	missing := httptest.NewServer(http.NotFoundHandler())
	defer missing.Close()

	blob := cacheArchive(t, map[string]string{"value": "from cache"})
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/blob/cached" {
			http.NotFound(w, r)

			return
		}

		_, _ = w.Write(blob)
	}))
	defer good.Close()

	cache := &packageCache{
		endpoints: []string{missing.URL, good.URL},
		http:      &http.Client{},
	}
	root := t.TempDir()
	out := filepath.Join(root, "out")
	cache.restore("cached", out, filepath.Join(root, "trash"))
	data, err := os.ReadFile(filepath.Join(out, "value"))

	if err != nil || string(data) != "from cache" {
		t.Fatalf("data=%q err=%v", data, err)
	}
}

func TestPackageCacheRestoreStreamsAndResetsPartialOutput(t *testing.T) {
	brokenBlob := cacheArchive(t, map[string]string{"stale": "partial"})
	brokenTail := cacheArchive(t, map[string]string{"ignored": "truncated"})
	brokenBlob = append(brokenBlob, brokenTail[:len(brokenTail)-1]...)
	goodBlob := cacheArchive(t, map[string]string{"value": "ready"})
	var brokenCalls atomic.Int32
	var goodCalls atomic.Int32

	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		brokenCalls.Add(1)
		_, _ = w.Write(brokenBlob)
	}))
	defer broken.Close()

	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodCalls.Add(1)
		_, _ = w.Write(goodBlob)
	}))
	defer good.Close()

	cache := &packageCache{
		endpoints: []string{broken.URL, good.URL},
		http:      &http.Client{},
	}
	root := t.TempDir()
	out := filepath.Join(root, "out")
	trash := filepath.Join(root, "trash")

	if err := os.MkdirAll(trash, 0755); err != nil {
		t.Fatal(err)
	}

	cache.restore("cached", out, trash)

	if brokenCalls.Load() != 1 || goodCalls.Load() != 1 {
		t.Fatalf("endpoint calls: broken=%d good=%d", brokenCalls.Load(), goodCalls.Load())
	}

	data, err := os.ReadFile(filepath.Join(out, "value"))

	if err != nil || string(data) != "ready" {
		t.Fatalf("restored output=%q err=%v", data, err)
	}

	if _, err := os.Stat(filepath.Join(out, "stale")); !os.IsNotExist(err) {
		t.Fatalf("partial output survived retry: %v", err)
	}

	partialFound := false
	err = filepath.WalkDir(trash, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if strings.HasSuffix(entry.Name(), ".tar.zst") {
			t.Fatalf("cache archive written to trash: %s", path)
		}

		if entry.Name() == "stale" {
			partialFound = true
		}

		return nil
	})

	if err != nil {
		t.Fatal(err)
	}

	if !partialFound {
		t.Fatal("broken archive did not produce partial output")
	}
}

func TestPackageCacheNotFoundEverywhereIsTerminal(t *testing.T) {
	missing1 := httptest.NewServer(http.NotFoundHandler())
	defer missing1.Close()
	missing2 := httptest.NewServer(http.NotFoundHandler())
	defer missing2.Close()

	cache := &packageCache{
		endpoints: []string{missing1.URL, missing2.URL},
		http:      &http.Client{},
	}
	root := t.TempDir()
	exc := Try(func() {
		cache.restore("gone", filepath.Join(root, "out"), filepath.Join(root, "trash"))
	})

	if exc == nil || !strings.Contains(exc.Error(), "not found on any endpoint") {
		t.Fatalf("exception=%v", exc)
	}
}

func TestCachedNodeSkipsDependencyTraversal(t *testing.T) {
	blob := cacheArchive(t, map[string]string{"value": "ready"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/resolve":
			_ = json.NewEncoder(w).Encode([]string{"target-uid"})
		case "/v1/blob/target-uid":
			_, _ = w.Write(blob)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	root := t.TempDir()
	dep := filepath.Join(root, "dep")
	target := filepath.Join(root, "target")
	t.Setenv("IX_PACKAGE_CACHE", server.URL)

	graph := &Graph{
		Nodes: []Node{
			{
				UID:     "dep-uid",
				OutDirs: []string{dep},
				Pool:    "threads",
				Cmds:    []Cmd{{Args: []string{"/bin/false"}}},
			},
			{
				UID:     "target-uid",
				InDirs:  []string{dep},
				OutDirs: []string{target},
				Pool:    "threads",
			},
		},
		Targets:  []string{target + "/touch"},
		Pools:    map[string]int{"threads": 1, "network": 1},
		TrashDir: filepath.Join(root, "trash"),
	}

	if graph.execute() {
		t.Fatal("cached graph failed")
	}

	if _, err := os.Stat(dep + "/touch"); !os.IsNotExist(err) {
		t.Fatalf("dependency unexpectedly traversed: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(target, "value"))

	if err != nil || string(data) != "ready" {
		t.Fatalf("cached output=%q err=%v", data, err)
	}
}
