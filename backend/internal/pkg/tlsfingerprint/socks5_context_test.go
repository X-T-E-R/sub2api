package tlsfingerprint

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"testing"
	"time"
)

func TestSOCKS5TunnelHandshakeRespectsContext(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		name := "cancel"
		if deadline {
			name = "deadline"
		}
		t.Run(name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			greeting := make(chan struct{})
			release := make(chan struct{})
			defer close(release)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				if _, err := io.ReadFull(conn, make([]byte, 3)); err != nil {
					return
				}
				close(greeting)
				<-release // Accept TCP but never finish SOCKS negotiation.
			}()
			ctx, cancel := context.WithCancel(context.Background())
			want := context.Canceled
			if deadline {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 150*time.Millisecond)
				want = context.DeadlineExceeded
			}
			defer cancel()
			proxyURL := &url.URL{Scheme: "socks5h", Host: listener.Addr().String()}
			result := make(chan error, 1)
			go func() {
				conn, err := NewSOCKS5ProxyDialer(nil, proxyURL).DialTLSContext(ctx, "tcp", "unresolved.invalid:443")
				if conn != nil {
					conn.Close()
				}
				result <- err
			}()
			select {
			case <-greeting:
			case <-time.After(time.Second):
				t.Fatal("SOCKS greeting not received")
			}
			if !deadline {
				cancel()
			}
			select {
			case err := <-result:
				// x/net may report the interrupted socket's timeout rather than
				// wrapping ctx.Err(); timely interruption is the dialer contract.
				if err == nil || !errors.Is(ctx.Err(), want) {
					t.Fatalf("got error=%v context=%v, want cancellation=%v", err, ctx.Err(), want)
				}
			case <-time.After(time.Second):
				t.Fatal("SOCKS negotiation ignored context")
			}
		})
	}
}
