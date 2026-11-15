// tls-fingerprint-probe verifies the built-in Node.js TLS profile without
// credentials or inference. It only sends GET to Google's discovery endpoint.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	utls "github.com/refraction-networking/utls"
)

func main() {
	proxyURL := flag.String("proxy", os.Getenv("SUB2_TLS_FINGERPRINT_PROBE_PROXY"), "SOCKS5/SOCKS5h URL; defaults to SUB2_TLS_FINGERPRINT_PROBE_PROXY, empty uses direct TCP")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var dial func(context.Context, string, string) (net.Conn, error)
	route := "direct"
	if *proxyURL == "" {
		dial = tlsfingerprint.NewDialer(nil, nil).DialTLSContext
	} else {
		u, err := url.Parse(*proxyURL)
		if err != nil || (u.Scheme != "socks5" && u.Scheme != "socks5h") || u.Host == "" {
			fmt.Fprintln(os.Stderr, "proxy must be a SOCKS5/SOCKS5h URL")
			os.Exit(1)
		}
		route = u.Scheme + "://" + u.Host
		dial = tlsfingerprint.NewSOCKS5ProxyDialer(nil, u).DialTLSContext
	}
	var version, cipher uint16
	var alpn string
	transport := &http.Transport{DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dial(ctx, network, addr)
		if err == nil {
			state := conn.(*utls.UConn).ConnectionState()
			version, cipher, alpn = state.Version, state.CipherSuite, state.NegotiatedProtocol
		}
		return conn, err
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	const target = "https://daily-cloudcode-pa.googleapis.com/$discovery/rest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		panic(err)
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", route, err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
		"route": route, "method": http.MethodGet, "url": target,
		"status": resp.StatusCode, "tls_version": fmt.Sprintf("0x%04x", version),
		"cipher_suite": fmt.Sprintf("0x%04x", cipher), "alpn": alpn,
		"elapsed_ms": time.Since(start).Milliseconds(), "oauth": false, "inference": false,
	})
}
