package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"strings"
	"time"
)

func fetchConnectTimeout(timeout time.Duration) time.Duration {
	return time.Duration(int64(0.1*float64(timeout/time.Second))) * time.Second
}

func fetchProxy(raw string) *url.URL {
	if !strings.Contains(raw, "://") {
		raw = "socks5://" + raw
	}
	u := Throw2(url.Parse(raw))
	if u.Port() == "" {
		u.Host = net.JoinHostPort(u.Hostname(), "1080")
	}
	return u
}

func fetchHTTP(rawURL, path, proxy string, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	connectTimeout := fetchConnectTimeout(timeout)
	transport := &http.Transport{
		DisableCompression: true,                                  // Hash raw bytes, not transparently decoded HTTP gzip.
		TLSClientConfig:    &tls.Config{InsecureSkipVerify: true}, // Preserve curl -k.
		Proxy:              http.ProxyFromEnvironment,
		DialContext: func(_ context.Context, network, address string) (net.Conn, error) {
			var conn net.Conn
			err := Try(func() {
				connectCtx, stop := context.WithTimeout(ctx, connectTimeout)
				defer stop()
				conn = Throw2((&net.Dialer{}).DialContext(connectCtx, network, address))
				deadline, _ := connectCtx.Deadline()
				if err := conn.SetDeadline(deadline); err != nil {
					conn.Close()
					Throw(err)
				}
			})
			return conn, err.AsError()
		},
	}
	if proxy != "" {
		// net/http handles the SOCKS handshake and resolves names remotely.
		transport.Proxy = http.ProxyURL(fetchProxy(proxy))
	}
	defer transport.CloseIdleConnections()
	// Unlike separate Dial/TLS timeouts, this gives the entire connection
	// setup (DNS, TCP, proxy and TLS) ONE budget, including each redirect.
	var connectTimer *time.Timer
	defer func() {
		if connectTimer != nil {
			connectTimer.Stop()
		}
	}()
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GetConn: func(string) {
			if connectTimer != nil {
				connectTimer.Stop()
			}
			connectTimer = time.AfterFunc(connectTimeout, cancel)
		},
		GotConn: func(info httptrace.GotConnInfo) {
			connectTimer.Stop()
			// Once connected, only the total attempt timeout applies.
			if err := info.Conn.SetDeadline(time.Time{}); err != nil {
				cancel()
			}
		},
	})
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 50 {
				return fmt.Errorf("stopped after 50 redirects")
			}
			if req.URL.Scheme != via[0].URL.Scheme || req.URL.Host != via[0].URL.Host {
				req.Header.Del("Authorization")
			}
			return nil
		},
	}
	req := Throw2(http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil))
	req.Header.Set("Accept", "*/*")
	if strings.Contains(rawURL, "ghcr.io") {
		req.Header.Set("Authorization", "Bearer QQ==")
	}
	resp := Throw2(client.Do(req))
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		ThrowFmt("The requested URL returned error: %d", resp.StatusCode)
	}
	out := Throw2(os.Create(path))
	defer out.Close()
	Try(func() {
		Throw2(io.Copy(out, resp.Body))
		Throw(out.Close())
	}).Catch(func(exc *Exception) {
		// curl --remove-on-error; external timeout used to leave partials.
		if ctx.Err() == nil {
			_ = os.Remove(path)
		}
		exc.throw()
	})
}
