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

	cache := newPackageCache(failed.URL+","+good.URL, []Node{{UID: "cached"}, {UID: "local"}})

	if !cache.has("cached") || cache.has("local") {
		t.Fatalf("available=%v", cache.available)
	}

	if failedCalls.Load() != 1 {
		t.Fatalf("failed endpoint calls=%d", failedCalls.Load())
	}
}

func TestPackageCacheFetchDropsNotFoundEndpoint(t *testing.T) {
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
	archive := cache.fetch("cached", t.TempDir())
	defer os.Remove(archive)

	out := filepath.Join(t.TempDir(), "out")

	if err := os.MkdirAll(out, 0755); err != nil {
		t.Fatal(err)
	}

	extractArchive(archive, out)
	data, err := os.ReadFile(filepath.Join(out, "value"))

	if err != nil || string(data) != "from cache" {
		t.Fatalf("data=%q err=%v", data, err)
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
	exc := Try(func() {
		cache.fetch("gone", t.TempDir())
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
