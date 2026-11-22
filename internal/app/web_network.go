package app

import (
	"context"
	"crypto/tls"
	"net"
	"net/netip"
	"time"
)

func webLookup(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

func webDial(ctx context.Context, network, address string) (net.Conn, error) {
	dialer := net.Dialer{Timeout: 15 * time.Second}
	return dialer.DialContext(ctx, network, address)
}

func webTLSConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12}
}
