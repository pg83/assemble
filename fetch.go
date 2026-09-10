package main

import (
	"crypto/sha256"
	"flag"
	"fmt"
	"io"
	"iter"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type fetchSource struct {
	url   string
	cache bool
}

// Python's dict preserves the first insertion's position, but the last
// assignment's value. In particular, an origin also listed as a mirror is
// still an origin, at the mirror's position.
type fetchSources struct {
	order []string
	good  map[string]fetchSource
}

func newFetchSources(origin, sha string, mirrors []string) *fetchSources {
	s := &fetchSources{good: make(map[string]fetchSource)}
	add := func(url string, cache bool) {
		if _, exists := s.good[url]; !exists {
			s.order = append(s.order, url)
		}
		s.good[url] = fetchSource{url, cache}
	}
	if len(sha) == 64 && sha != strings.Repeat("1", 64) {
		for _, mirror := range mirrors {
			url := strings.ReplaceAll(mirror, "{sha}", sha)
			url = strings.ReplaceAll(url, "{two}", sha[:2])
			url = strings.ReplaceAll(url, "{one}", sha[:1])
			add(url, true)
		}
	}
	add(origin, false)
	return s
}

func (s *fetchSources) urls() iter.Seq[fetchSource] {
	return func(yield func(fetchSource) bool) {
		for len(s.good) != 0 {
			// Keep the snapshot semantics of yield from list(good.items()).
			var batch []fetchSource
			for _, url := range s.order {
				if source, ok := s.good[url]; ok {
					batch = append(batch, source)
				}
			}
			for _, source := range batch {
				if !yield(source) {
					return
				}
			}
		}
	}
}

func fetchTimeouts(random func() float64) iter.Seq[time.Duration] {
	return func(yield func(time.Duration) bool) {
		for tout := 60.0; ; tout = math.Min(tout*1.5, 10000) {
			// Python: int(tout * (0.5 + random.random())), in seconds.
			if !yield(time.Duration(int64(tout*(0.5+random()))) * time.Second) {
				return
			}
		}
	}
}

func fetchRoutes(raw string) iter.Seq[string] {
	return func(yield func(string) bool) {
		for {
			for _, item := range strings.Split(strings.TrimSpace(raw), ";") {
				if proxy := strings.TrimSpace(item); proxy != "" {
					if !yield(proxy) {
						return
					}
				}
			}
			if !yield("") {
				return
			}
		}
	}
}

type fetcher struct {
	random   func() float64
	download func(url, path, proxy string, timeout time.Duration)
	log      io.Writer
}

func prepareFetchDir(path string) {
	// A bare filename (bld/fetch's use case) must not clean the cwd.
	if !strings.ContainsRune(path, filepath.Separator) {
		return
	}
	dir := filepath.Dir(path)
	absolute := Throw2(filepath.Abs(dir))
	cwd := Throw2(os.Getwd())
	if absolute == string(filepath.Separator) || absolute == cwd {
		ThrowFmt("refusing to clean fetch directory %q", dir)
	}
	// shutil.rmtree rejects directory symlinks; don't follow or replace them.
	if info, err := os.Lstat(dir); err == nil {
		if !info.IsDir() {
			ThrowFmt("fetch directory is not a directory: %s", dir)
		}
	} else if !os.IsNotExist(err) {
		Throw(err)
	}
	Throw(os.RemoveAll(dir))
	Throw(os.MkdirAll(dir, 0755))
}

func checkFetchSHA(path, expected string) {
	if strings.Contains(expected, "__skip__") {
		return
	}
	f := Throw2(os.Open(path))
	defer f.Close()
	h := sha256.New()
	Throw2(io.Copy(h, f))
	actual := fmt.Sprintf("%x", h.Sum(nil))
	if actual != expected {
		ThrowFmt("got %s checksum, not %s", actual, expected)
	}
}

func (f *fetcher) fetch(origin, path, sha string, mirrors []string, proxies string) {
	sources := newFetchSources(origin, sha, mirrors)
	nextTimeout, stopTimeout := iter.Pull(fetchTimeouts(f.random))
	defer stopTimeout()
	nextRoute, stopRoute := iter.Pull(fetchRoutes(proxies))
	defer stopRoute()
	// Deliberately zip three independent streams, NOT nested URL/proxy loops.
	for source := range sources.urls() {
		timeout, _ := nextTimeout()
		proxy, _ := nextRoute()
		prepareFetchDir(path) // Outside the retry Try, as in Python.
		fmt.Fprintf(f.log, "fetch %s via %q timeout=%s connect-timeout=%s\n", source.url, proxy, timeout, fetchConnectTimeout(timeout))
		err := Try(func() {
			f.download(source.url, path, proxy, timeout)
			checkFetchSHA(path, sha)
		})
		if err == nil {
			return
		}
		switch {
		case strings.Contains(err.Error(), "error: 404"):
			fmt.Fprintf(f.log, "404 %s\n", source.url)
			delete(sources.good, source.url)
		case strings.Contains(err.Error(), "checksum"):
			if !source.cache {
				err.throw()
			}
			delete(sources.good, source.url)
		default:
			fmt.Fprintf(f.log, "while fetch %s: %v\n", source.url, err)
		}
	}
	ThrowFmt("can not fetch %s, no sources left", origin)
}

func cliFetch(args []string) {
	flags := flag.NewFlagSet("assemble fetch", flag.ContinueOnError)
	mirrorsRaw := flags.String("mirrors", "", "newline-separated mirror URL templates")
	proxies := flags.String("socks5", "", "semicolon-separated SOCKS5 proxies (remote DNS)")
	Throw(flags.Parse(args))
	if flags.NArg() != 3 {
		ThrowFmt("usage: assemble fetch [-mirrors URLs] [-socks5 proxies] URL PATH SHA")
	}
	var mirrors []string
	if raw := strings.TrimSpace(*mirrorsRaw); raw != "" {
		mirrors = strings.Split(raw, "\n")
	}
	rand.Shuffle(len(mirrors), func(i, j int) { mirrors[i], mirrors[j] = mirrors[j], mirrors[i] })
	// The original mirrors * 3 is retained; dict construction deduplicates it.
	mirrors = append(append(append([]string{}, mirrors...), mirrors...), mirrors...)
	f := fetcher{random: rand.Float64, download: fetchHTTP, log: os.Stderr}
	f.fetch(flags.Arg(0), flags.Arg(1), strings.TrimPrefix(flags.Arg(2), "sha:"), mirrors, *proxies)
}
