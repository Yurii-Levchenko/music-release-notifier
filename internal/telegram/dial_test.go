package telegram

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"
)

// The server has no public IPv4 address, so Telegram is reachable only over
// IPv6. A listener on IPv6 loopback alone stands in for that: "localhost"
// resolves to both families, and only the IPv6 one answers.
func TestDialReachesAnIPv6OnlyHost(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback here: %v", err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			_ = c.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	addr := net.JoinHostPort("localhost", strconv.Itoa(port))

	conn, err := dialBothFamilies(addr, 5*time.Second)
	if err != nil {
		t.Fatalf("could not reach an IPv6-only host: %v", err)
	}
	_ = conn.Close()
}
