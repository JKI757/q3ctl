// Package rcon is the small UDP client q3ctl uses for allowlisted operations.
package rcon

import (
	"context"
	"net"
	"strings"
	"time"
)

type Client struct{ Address, Password string }

func (c Client) Execute(ctx context.Context, command string) (string, error) {
	dialer := net.Dialer{Timeout: 2 * time.Second}
	conn, err := dialer.DialContext(ctx, "udp", c.Address)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	deadline := time.Now().Add(3 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err = conn.SetDeadline(deadline); err != nil {
		return "", err
	}
	if _, err = conn.Write([]byte("\xff\xff\xff\xffrcon " + c.Password + " " + command + "\n")); err != nil {
		return "", err
	}
	buf := make([]byte, 16384)
	n, err := conn.Read(buf)
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(string(buf[:n]), "\xff\xff\xff\xffprint\n"), nil
}

// GetStatus queries Quake's standard unauthenticated status protocol. ioquake3
// consistently returns the server-info cvars (including mapname and
// g_gametype) here, while the authenticated RCON `status` response varies by
// build and is retained separately for its player table.
func (c Client) GetStatus(ctx context.Context) (string, error) {
	dialer := net.Dialer{Timeout: 2 * time.Second}
	conn, err := dialer.DialContext(ctx, "udp", c.Address)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	deadline := time.Now().Add(3 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err = conn.SetDeadline(deadline); err != nil {
		return "", err
	}
	if _, err = conn.Write([]byte("\xff\xff\xff\xffgetstatus\n")); err != nil {
		return "", err
	}
	buf := make([]byte, 16384)
	n, err := conn.Read(buf)
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(string(buf[:n]), "\xff\xff\xff\xffstatusResponse\n"), nil
}
