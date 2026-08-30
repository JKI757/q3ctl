package rcon

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestGetStatus(t *testing.T) {
	listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		buf := make([]byte, 1024)
		n, addr, err := listener.ReadFromUDP(buf)
		if err != nil || string(buf[:n]) != "\xff\xff\xff\xffgetstatus\n" {
			return
		}
		_, _ = listener.WriteToUDP([]byte("\xff\xff\xff\xffstatusResponse\n\\mapname\\actf01\\g_gametype\\4\n"), addr)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := (Client{Address: listener.LocalAddr().String()}).GetStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := "\\mapname\\actf01\\g_gametype\\4\n"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
