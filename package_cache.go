package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
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
	available map[string]string
	http      *http.Client
	sleep     func(time.Duration)
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

	if r.current > 1000*time.Second {
		r.current = 1000 * time.Second
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

func isLoopbackEndpoint(endpoint string) bool {
	parsed, err := url.Parse(endpoint)

	if err != nil {
		return false
	}

	host := parsed.Hostname()

	return host == "localhost" || host == "::1" || strings.HasPrefix(host, "127.")
}

func newPackageCache(raw string, nodes []Node) *packageCache {
	cache := &packageCache{
		endpoints: parseCacheEndpoints(raw),
		available: map[string]string{},
		http:      &http.Client{},
		sleep:     time.Sleep,
	}

	if len(cache.endpoints) == 0 {
		return cache
	}

	// Loopback endpoints stay in front so a co-hosted cache is always tried
	// first; only the remote tail is shuffled for load spreading.
	local := 0

	for index, endpoint := range cache.endpoints {
		if isLoopbackEndpoint(endpoint) {
			cache.endpoints[local], cache.endpoints[index] = cache.endpoints[index], cache.endpoints[local]
			local++
		}
	}

	remote := cache.endpoints[local:]

	rand.Shuffle(len(remote), func(i, j int) {
		remote[i], remote[j] = remote[j], remote[i]
	})

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
	if c == nil || uid == "" {
		return false
	}

	_, ok := c.available[uid]

	return ok
}

type cacheResolveResult struct {
	endpoint  string
	available map[string]string
	err       error
	infra     bool
}

func cacheInfraStatus(code int) bool {
	return code == http.StatusTooManyRequests || code >= 500
}

func (c *packageCache) resolveFrom(ctx context.Context, endpoint string, payload []byte) cacheResolveResult {
	result := cacheResolveResult{endpoint: endpoint}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/v2/resolve", bytes.NewReader(payload))

	if err != nil {
		result.err = err

		return result
	}

	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)

	if err != nil {
		result.err = err
		result.infra = true

		return result
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		result.err = fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		result.infra = cacheInfraStatus(resp.StatusCode)

		return result
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, (64<<20)+1))
	closeErr := resp.Body.Close()

	if err != nil || closeErr != nil {
		result.err = fmt.Errorf("bad response: %v %v", err, closeErr)
		result.infra = true

		return result
	}

	if len(body) > 64<<20 {
		result.err = fmt.Errorf("response exceeds 64 MiB")

		return result
	}

	if err := json.Unmarshal(body, &result.available); err != nil {
		result.err = fmt.Errorf("bad response: %v", err)
	}

	return result
}

func (c *packageCache) resolve(uids []string) map[string]string {
	payload := Throw2(json.Marshal(uids))
	timeouts := &retryTimeouts{current: 10 * time.Second}
	good := append([]string{}, c.endpoints...)
	attempt := 0
	index := 0

	for len(good) > 0 {
		if index >= len(good) {
			index = 0
		}

		endpoint := good[index]
		tout := timeouts.next()

		if attempt > 0 {
			c.sleep(tout / 10)
		}

		attempt++
		ctx, cancel := context.WithTimeout(context.Background(), tout)
		resolved := c.resolveFrom(ctx, endpoint, payload)
		cancel()

		if resolved.err != nil {
			if resolved.infra {
				fmt.Fprintf(os.Stderr, "package cache resolve %s: infrastructure error: %v, cycling endpoints\n",
					resolved.endpoint, resolved.err)
				index++

				continue
			}

			fmt.Fprintf(os.Stderr, "package cache resolve %s: permanent error: %v, dropping endpoint\n",
				resolved.endpoint, resolved.err)
			good = removeEndpoint(good, index)

			continue
		}

		fmt.Fprintf(os.Stderr, "package cache: resolved %d/%d nodes via %s\n",
			len(resolved.available), len(uids), resolved.endpoint)

		return resolved.available
	}

	fmt.Fprintln(os.Stderr, "package cache: no usable resolve endpoints, continuing without cache")

	return map[string]string{}
}

func removeEndpoint(items []string, index int) []string {
	return append(items[:index], items[index+1:]...)
}

type countingReader struct {
	io.Reader
	n int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.n += int64(n)

	return n, err
}

func (c *packageCache) restore(uid, outDir, trashDir string) {
	expected := c.available[uid]
	good := append([]string{}, c.endpoints...)
	timeouts := &retryTimeouts{}
	index := 0
	prepareDir(trashDir, outDir)

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

		digest := md5.New()
		body := &countingReader{Reader: io.TeeReader(resp.Body, digest)}
		extractErr := Try(func() {
			extractArchive(body, outDir)
		})
		bodyErr := resp.Body.Close()
		cancel()

		if extractErr != nil || bodyErr != nil || (resp.ContentLength >= 0 && body.n != resp.ContentLength) {
			prepareDir(trashDir, outDir)
			fmt.Fprintf(os.Stderr, "package cache fetch %s from %s: bad archive (%d/%d): %v %v, cycling endpoints\n",
				uid, endpoint, body.n, resp.ContentLength, extractErr, bodyErr)
			index++

			continue
		}

		if got := hex.EncodeToString(digest.Sum(nil)); expected != "" && got != expected {
			prepareDir(trashDir, outDir)
			fmt.Fprintf(os.Stderr, "package cache fetch %s from %s: md5 %s, want %s, cycling endpoints\n",
				uid, endpoint, got, expected)
			index++

			continue
		}

		return
	}

	ThrowFmt("package cache uid %s: not found on any endpoint", uid)
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

func extractArchive(input io.Reader, outDir string) {
	decoder := Throw2(zstd.NewReader(input))
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

	_, err := io.Copy(io.Discard, decoder)
	Throw(err)
}

func (self *executor) executeCached(node *Node, out io.Writer) {
	if len(node.OutDirs) != 1 {
		ThrowFmt("package cache node %s has %d out dirs, want 1", node.UID, len(node.OutDirs))
	}

	outDir := node.OutDirs[0]

	exc := Try(func() {
		self.cache.restore(node.UID, outDir, self.trashDir)
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
