package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestFetchCLICompatibility(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()
	for _, sha := range []string{fmt.Sprintf("sha:%x", sha256.Sum256([]byte("ok"))), "prefix__skip__suffix"} {
		cliFetch([]string{server.URL, filepath.Join(t.TempDir(), "out", "file"), sha})
	}
	err := Try(func() {
		cliFetch([]string{server.URL + "/missing", filepath.Join(t.TempDir(), "out", "file"), strings.Repeat("a", 64)})
	})
	if err == nil || !strings.Contains(err.Error(), "no sources left") {
		t.Fatal(err)
	}
}

func TestFetchHTTPRawBytesAndRedirects(t *testing.T) {
	var compressed bytes.Buffer
	w := gzip.NewWriter(&compressed)
	Throw2(w.Write([]byte("artifact")))
	Throw(w.Close())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept-Encoding") != "" {
			t.Error("automatic compression requested")
		}
		if strings.HasPrefix(r.URL.Path, "/redirect/") {
			n := Throw2(strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/redirect/")))
			if n > 0 {
				http.Redirect(w, r, "/redirect/"+strconv.Itoa(n-1), 302)
				return
			}
		}
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(compressed.Bytes())
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "file")
	fetchHTTP(server.URL+"/redirect/50", path, "", 30*time.Second)
	if got := Throw2(os.ReadFile(path)); !bytes.Equal(got, compressed.Bytes()) {
		t.Fatal("HTTP gzip was decoded")
	}
	if err := Try(func() { fetchHTTP(server.URL+"/redirect/51", path, "", 30*time.Second) }); err == nil || !strings.Contains(err.Error(), "50 redirects") {
		t.Fatal(err)
	}
}

func TestFetchHTTPGHCRHeaderAndRedirectScope(t *testing.T) {
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("authorization leaked across origins")
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer QQ==" {
			t.Error("missing GHCR header")
		}
		if r.URL.Path == "/ghcr.io" {
			http.Redirect(w, r, "/same-origin", 302)
		} else {
			http.Redirect(w, r, destination.URL, 302)
		}
	}))
	defer source.Close()
	fetchHTTP(source.URL+"/ghcr.io", filepath.Join(t.TempDir(), "file"), "", 30*time.Second)
}

func TestFetchHTTPStatusAndPartialFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/partial" {
			w.Header().Set("Content-Length", "100")
			_, _ = io.WriteString(w, "short")
		} else {
			w.WriteHeader(Throw2(strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/"))))
		}
	}))
	defer server.Close()
	for _, code := range []string{"404", "403", "429", "500", "partial"} {
		path := filepath.Join(t.TempDir(), "file")
		err := Try(func() { fetchHTTP(server.URL+"/"+code, path, "", 30*time.Second) })
		if err == nil {
			t.Fatal("accepted failure", code)
		}
		if code != "partial" && !strings.Contains(err.Error(), "error: "+code) {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("partial file remains", err)
		}
	}
}

// A test SOCKS peer deliberately receives an unresolvable domain and forwards
// it to a local server. This proves the production client uses remote DNS.
func fetchTestSOCKS(t *testing.T, upstream string, greetingDelay time.Duration) (string, <-chan string) {
	t.Helper()
	listener := Throw2(net.Listen("tcp", "127.0.0.1:0"))
	t.Cleanup(func() { listener.Close() })
	hosts := make(chan string, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
		errExc := Try(func() {
			var greeting [2]byte
			Throw2(io.ReadFull(conn, greeting[:]))
			Throw2(io.CopyN(io.Discard, conn, int64(greeting[1])))
			time.Sleep(greetingDelay)
			Throw2(conn.Write([]byte{5, 0}))
			var request [5]byte
			Throw2(io.ReadFull(conn, request[:]))
			if request[0] != 5 || request[1] != 1 || request[3] != 3 {
				ThrowFmt("expected domain SOCKS request: %v", request)
			}
			host := make([]byte, int(request[4]))
			Throw2(io.ReadFull(conn, host))
			Throw2(io.CopyN(io.Discard, conn, 2))
			hosts <- string(host)
			dst := Throw2(net.Dial("tcp", upstream))
			defer dst.Close()
			Throw2(conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0}))
			done := make(chan struct{})
			go func() { _, _ = io.Copy(dst, conn); dst.Close(); close(done) }()
			_, _ = io.Copy(conn, dst)
			conn.Close()
			<-done
		})
		if errExc != nil {
			select {
			case hosts <- "error: " + errExc.Error():
			default:
			}
		}
	}()
	return listener.Addr().String(), hosts
}

func TestFetchHTTPSViaSOCKSRemoteDNS(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "via proxy") }))
	defer server.Close()
	proxy, hosts := fetchTestSOCKS(t, server.Listener.Addr().String(), 0)
	path := filepath.Join(t.TempDir(), "file")
	fetchHTTP("https://fetch-test.invalid/artifact", path, proxy, 30*time.Second)
	if host := <-hosts; host != "fetch-test.invalid" {
		t.Fatal(host)
	}
	if got := string(Throw2(os.ReadFile(path))); got != "via proxy" {
		t.Fatal(got)
	}
}

func TestFetchConnectionBudgetIncludesSOCKSAndTLS(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	server.TLS = &tls.Config{GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) { time.Sleep(700 * time.Millisecond); return nil, nil }}
	server.StartTLS()
	defer server.Close()
	proxy, _ := fetchTestSOCKS(t, server.Listener.Addr().String(), 700*time.Millisecond)
	start := time.Now()
	err := Try(func() {
		fetchHTTP("https://fetch-test.invalid/file", filepath.Join(t.TempDir(), "file"), proxy, 10*time.Second)
	})
	elapsed := time.Since(start)
	if err == nil || elapsed < 800*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("elapsed=%s error=%v", elapsed, err)
	}
}

func TestFetchConnectionBudgetEndsBeforeBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		time.Sleep(1200 * time.Millisecond)
		_, _ = io.WriteString(w, "slow body")
	}))
	defer server.Close()
	fetchHTTP(server.URL, filepath.Join(t.TempDir(), "file"), "", 10*time.Second)
}

func TestFetchTotalBudgetIncludesBody(t *testing.T) {
	if testing.Short() {
		t.Skip("10 second total timeout")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "partial body")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "file")
	start := time.Now()
	err := Try(func() { fetchHTTP(server.URL, path, "", 10*time.Second) })
	if err == nil || time.Since(start) < 9*time.Second || time.Since(start) > 12*time.Second {
		t.Fatalf("elapsed=%s error=%v", time.Since(start), err)
	}
	if got := string(Throw2(os.ReadFile(path))); got != "partial body" {
		t.Fatal(got)
	}
}
