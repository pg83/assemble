package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/klauspost/compress/zstd"
)

type packageCache struct {
	endpoints []string
	available map[string]bool
	http      *http.Client
}

type retryTimeouts struct {
	current time.Duration
}

func (r *retryTimeouts) next() time.Duration {
	if r.current == 0 {
		r.current = 60 * time.Second
	}

	result := time.Duration(float64(r.current) * (0.5 + rand.Float64()))
	r.current = time.Duration(float64(r.current) * 1.5)

	if r.current > 10000*time.Second {
		r.current = 10000 * time.Second
	}

	return result
}

func parseCacheEndpoints(raw string) []string {
	var result []string

	for _, item := range strings.Split(raw, ",") {
		endpoint := strings.TrimSpace(item)

		if endpoint == "" {
			continue
		}

		if !strings.Contains(endpoint, "://") {
			endpoint = "http://" + endpoint
		}

		result = append(result, strings.TrimRight(endpoint, "/"))
	}

	return result
}

func newPackageCache(raw string, nodes []Node) *packageCache {
	cache := &packageCache{
		endpoints: parseCacheEndpoints(raw),
		available: map[string]bool{},
		http:      &http.Client{},
	}

	if len(cache.endpoints) == 0 {
		return cache
	}

	uids := make([]string, 0, len(nodes))

	for _, node := range nodes {
		if node.UID != "" {
			uids = append(uids, node.UID)
		}
	}

	cache.available = cache.resolve(uids)

	return cache
}

func (c *packageCache) has(uid string) bool {
	return c != nil && uid != "" && c.available[uid]
}

func (c *packageCache) resolve(uids []string) map[string]bool {
	payload := Throw2(json.Marshal(uids))
	timeouts := &retryTimeouts{}

	for attempt := 0; ; attempt++ {
		endpoint := c.endpoints[attempt%len(c.endpoints)]
		tout := timeouts.next()
		ctx, cancel := context.WithTimeout(context.Background(), tout)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/v1/resolve", bytes.NewReader(payload))

		if err != nil {
			cancel()
			Throw(err)
		}

		req.Header.Set("Content-Type", "application/json")
		resp, err := c.http.Do(req)

		if err != nil {
			cancel()
			fmt.Fprintf(os.Stderr, "package cache resolve %s: %v, cycling endpoints\n", endpoint, err)

			continue
		}

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			cancel()
			fmt.Fprintf(os.Stderr, "package cache resolve %s: HTTP %d: %s, cycling endpoints\n",
				endpoint, resp.StatusCode, strings.TrimSpace(string(body)))

			continue
		}

		var available []string
		err = json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&available)
		closeErr := resp.Body.Close()
		cancel()

		if err != nil || closeErr != nil {
			fmt.Fprintf(os.Stderr, "package cache resolve %s: bad response: %v %v, cycling endpoints\n",
				endpoint, err, closeErr)

			continue
		}

		result := make(map[string]bool, len(available))

		for _, uid := range available {
			result[uid] = true
		}

		fmt.Fprintf(os.Stderr, "package cache: resolved %d/%d nodes via %s\n", len(result), len(uids), endpoint)

		return result
	}
}

func removeEndpoint(items []string, index int) []string {
	return append(items[:index], items[index+1:]...)
}

func (c *packageCache) fetch(uid, trashDir string) string {
	good := append([]string{}, c.endpoints...)
	timeouts := &retryTimeouts{}
	index := 0

	for len(good) > 0 {
		if index >= len(good) {
			index = 0
		}

		endpoint := good[index]
		tout := timeouts.next()
		ctx, cancel := context.WithTimeout(context.Background(), tout)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/v1/blob/"+url.PathEscape(uid), nil)

		if err != nil {
			cancel()
			Throw(err)
		}

		resp, err := c.http.Do(req)

		if err != nil {
			cancel()
			fmt.Fprintf(os.Stderr, "package cache fetch %s from %s: %v, cycling endpoints\n", uid, endpoint, err)
			index++

			continue
		}

		if resp.StatusCode == http.StatusNotFound {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			cancel()
			fmt.Fprintf(os.Stderr, "package cache fetch %s from %s: not found, dropping endpoint\n", uid, endpoint)
			good = removeEndpoint(good, index)

			continue
		}

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			cancel()
			fmt.Fprintf(os.Stderr, "package cache fetch %s from %s: HTTP %d: %s, cycling endpoints\n",
				uid, endpoint, resp.StatusCode, strings.TrimSpace(string(body)))
			index++

			continue
		}

		file := Throw2(os.CreateTemp(trashDir, "assemble-cache-*.tar.zst"))
		path := file.Name()
		written, copyErr := io.Copy(file, resp.Body)
		bodyErr := resp.Body.Close()
		fileErr := file.Close()
		cancel()

		if copyErr != nil || bodyErr != nil || fileErr != nil || (resp.ContentLength >= 0 && written != resp.ContentLength) {
			_ = os.Remove(path)
			fmt.Fprintf(os.Stderr, "package cache fetch %s from %s: incomplete body (%d/%d): %v %v %v, cycling endpoints\n",
				uid, endpoint, written, resp.ContentLength, copyErr, bodyErr, fileErr)
			index++

			continue
		}

		return path
	}

	ThrowFmt("package cache uid %s: not found on any endpoint", uid)

	return ""
}

type dirMetadata struct {
	path    string
	mode    os.FileMode
	modTime time.Time
}

func tarMode(mode int64) os.FileMode {
	result := os.FileMode(mode & 0777)

	if mode&04000 != 0 {
		result |= os.ModeSetuid
	}

	if mode&02000 != 0 {
		result |= os.ModeSetgid
	}

	if mode&01000 != 0 {
		result |= os.ModeSticky
	}

	return result
}

func archivePath(root, name string) string {
	rel := filepath.Clean(filepath.FromSlash(name))

	if rel == "." {
		return root
	}

	if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		ThrowFmt("package cache archive path escapes output: %q", name)
	}

	current := root

	for _, part := range strings.Split(filepath.Dir(rel), string(filepath.Separator)) {
		if part == "." || part == "" {
			continue
		}

		current = filepath.Join(current, part)
		info, err := os.Lstat(current)

		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			ThrowFmt("package cache archive path traverses symlink: %q", name)
		}

		if err != nil && !os.IsNotExist(err) {
			Throw(err)
		}
	}

	return filepath.Join(root, rel)
}

func ensureParent(path string) {
	Throw(os.MkdirAll(filepath.Dir(path), 0755))
}

func rejectSymlink(path string) {
	info, err := os.Lstat(path)

	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		ThrowFmt("package cache archive entry replaces symlink: %q", path)
	}

	if err != nil && !os.IsNotExist(err) {
		Throw(err)
	}
}

func extractArchive(path, outDir string) {
	f := Throw2(os.Open(path))
	defer f.Close()

	decoder := Throw2(zstd.NewReader(f))
	defer decoder.Close()

	reader := tar.NewReader(decoder)
	var dirs []dirMetadata

	for {
		header, err := reader.Next()

		if errors.Is(err, io.EOF) {
			break
		}

		Throw(err)

		target := archivePath(outDir, header.Name)
		mode := tarMode(header.Mode)

		switch header.Typeflag {
		case tar.TypeDir:
			Throw(os.MkdirAll(target, 0755))
			dirs = append(dirs, dirMetadata{path: target, mode: mode, modTime: header.ModTime})
		case tar.TypeReg, tar.TypeRegA:
			ensureParent(target)
			rejectSymlink(target)
			file := Throw2(os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode))
			_, copyErr := io.CopyN(file, reader, header.Size)
			closeErr := file.Close()
			Throw(copyErr)
			Throw(closeErr)
			Throw(os.Chmod(target, mode))
			Throw(os.Chtimes(target, header.ModTime, header.ModTime))
		case tar.TypeSymlink:
			ensureParent(target)
			Throw(os.Symlink(header.Linkname, target))
		case tar.TypeLink:
			ensureParent(target)
			linkTarget := archivePath(outDir, header.Linkname)
			Throw(os.Link(linkTarget, target))
		case tar.TypeFifo:
			ensureParent(target)
			Throw(syscall.Mkfifo(target, uint32(header.Mode&0777)))
		case tar.TypeXHeader, tar.TypeXGlobalHeader, tar.TypeGNULongName, tar.TypeGNULongLink:
			continue
		default:
			ThrowFmt("package cache archive: unsupported tar entry type %d for %q", header.Typeflag, header.Name)
		}
	}

	for i := len(dirs) - 1; i >= 0; i-- {
		Throw(os.Chmod(dirs[i].path, dirs[i].mode))
		Throw(os.Chtimes(dirs[i].path, dirs[i].modTime, dirs[i].modTime))
	}
}

func (self *executor) executeCached(node *Node, out io.Writer) {
	if len(node.OutDirs) != 1 {
		ThrowFmt("package cache node %s has %d out dirs, want 1", node.UID, len(node.OutDirs))
	}

	outDir := node.OutDirs[0]
	prepareDir(self.trashDir, outDir)

	exc := Try(func() {
		archive := self.cache.fetch(node.UID, self.trashDir)
		defer os.Remove(archive)

		extractArchive(archive, outDir)
	})

	if exc != nil {
		moveToTrash(self.trashDir, outDir)
		exc.throw()
	}

	syscall.Sync()

	for _, output := range outs(node) {
		Throw2(os.Create(output)).Close()
		fmt.Fprintln(out, color(G, self.complete()+" LEAVE "+output+" (cache)"))
	}

	syscall.Sync()
}
