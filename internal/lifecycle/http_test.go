package lifecycle

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestRunHTTPShutsDownAfterCancellation(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_ = listener.Close()

	ctx, cancel := context.WithCancel(context.Background())
	server := &http.Server{Addr: listener.Addr().String(), Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})}
	done := make(chan error, 1)
	go func() {
		done <- RunHTTP(ctx, server, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()

	deadline := time.Now().Add(time.Second)
	for {
		response, requestErr := http.Get("http://" + server.Addr)
		if requestErr == nil {
			_ = response.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not start: %v", requestErr)
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunHTTP() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunHTTP did not stop")
	}
}

func TestRunHTTPRejectsInvalidArguments(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := RunHTTP(context.Background(), nil, time.Second, logger); err == nil {
		t.Fatal("nil server accepted")
	}
	if err := RunHTTP(context.Background(), &http.Server{}, 0, logger); err == nil {
		t.Fatal("zero timeout accepted")
	}
	if err := RunHTTP(context.Background(), &http.Server{}, time.Second, nil); err == nil {
		t.Fatal("nil logger accepted")
	}
}

func TestRunHTTPDrainsActiveRequest(t *testing.T) {
	addr := availableAddress(t)
	started := make(chan struct{})
	release := make(chan struct{})
	server := &http.Server{Addr: addr, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusNoContent)
	})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunHTTP(ctx, server, time.Second, discardLogger()) }()

	responseDone := make(chan error, 1)
	go func() {
		response, err := getEventually("http://"+addr, time.Second)
		if err == nil {
			_ = response.Body.Close()
		}
		responseDone <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("active request did not start")
	}
	cancel()
	select {
	case err := <-done:
		t.Fatalf("server stopped before request drained: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-responseDone; err != nil {
		t.Fatalf("active request failed during drain: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("RunHTTP() error = %v", err)
	}
}

func TestRunHTTPForcesCloseAfterShutdownTimeout(t *testing.T) {
	addr := availableAddress(t)
	started := make(chan struct{})
	handlerDone := make(chan struct{})
	server := &http.Server{Addr: addr, Handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(handlerDone)
	})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	const shutdownTimeout = 50 * time.Millisecond
	go func() { done <- RunHTTP(ctx, server, shutdownTimeout, discardLogger()) }()
	requestDone := make(chan struct{})
	go func() {
		response, _ := getEventually("http://"+addr, time.Second)
		if response != nil {
			_ = response.Body.Close()
		}
		close(requestDone)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("active request did not start")
	}
	start := time.Now()
	cancel()
	if err := <-done; err == nil {
		t.Fatal("RunHTTP() error = nil, want shutdown timeout")
	}
	if elapsed := time.Since(start); elapsed < shutdownTimeout {
		t.Fatalf("forced close after %s, want at least %s", elapsed, shutdownTimeout)
	}
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("forced close did not cancel active handler")
	}
	<-requestDone
}

func availableAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func getEventually(url string, timeout time.Duration) (*http.Response, error) {
	deadline := time.Now().Add(timeout)
	for {
		response, err := http.Get(url)
		if err == nil || time.Now().After(deadline) {
			return response, err
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
