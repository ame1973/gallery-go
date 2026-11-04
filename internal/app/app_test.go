package app

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestGracefulShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }), ReadHeaderTimeout: time.Second}
	done := make(chan error, 1)
	go func() { done <- serveUntilCancelled(ctx, server, listener) }()
	client := &http.Client{Timeout: time.Second}
	response, err := client.Get("http://" + listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 204 {
		t.Fatal(response.StatusCode)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not complete")
	}
	if response, err := client.Get("http://" + listener.Addr().String()); err == nil {
		response.Body.Close()
		t.Fatal("listener remained open")
	}
}
