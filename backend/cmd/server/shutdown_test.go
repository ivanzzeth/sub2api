package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type shutdownTestListener struct {
	net.Listener
	closed chan struct{}
	once   sync.Once
}

func (l *shutdownTestListener) Close() error {
	err := l.Listener.Close()
	l.once.Do(func() { close(l.closed) })
	return err
}

func TestShutdownTimeoutFromEnv(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  time.Duration
		valid bool
	}{{"", 90 * time.Second, true}, {"15m", 15 * time.Minute, true}, {"0", 0, false}, {"-1s", 0, false}, {"invalid", 0, false}} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("AI_ROUTER_SHUTDOWN_TIMEOUT", tc.value)
			got, err := shutdownTimeoutFromEnv()
			if (err == nil) != tc.valid || got != tc.want {
				t.Fatalf("got %v %v", got, err)
			}
		})
	}
}

func TestShutdownHTTPServerDrainsSSEAndPreservesDeadline(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "expired"}[expired], func(t *testing.T) {
			raw, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			ln := &shutdownTestListener{Listener: raw, closed: make(chan struct{})}
			release := make(chan struct{})
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if _, err := io.WriteString(w, "data: first\n\n"); err != nil {
					return
				}
				w.(http.Flusher).Flush()
				<-release
				if _, err := io.WriteString(w, "event: response.completed\ndata: [DONE]\n\n"); err != nil {
					return
				}
				w.(http.Flusher).Flush()
			})}
			served := make(chan error, 1)
			go func() { served <- server.Serve(ln) }()
			response, err := http.Get("http://" + ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				select {
				case <-release:
				default:
					close(release)
				}
				if err := response.Body.Close(); err != nil {
					t.Log(err)
				}
				if err := server.Close(); err != nil {
					t.Log(err)
				}
				select {
				case <-served:
				case <-time.After(5 * time.Second):
					t.Error("serve goroutine leaked")
				}
			})
			reader := bufio.NewReader(response.Body)
			line, err := reader.ReadString('\n')
			if err != nil || line != "data: first\n" {
				t.Fatalf("first event %q %v", line, err)
			}
			budget := 5 * time.Second
			if expired {
				budget = 0
			}
			stopped := make(chan error, 1)
			go func() { stopped <- shutdownHTTPServer(server, budget) }()
			<-ln.closed
			if expired {
				if err := <-stopped; !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("deadline=%v", err)
				}
			} else {
				select {
				case err := <-stopped:
					t.Fatalf("early Shutdown: %v", err)
				default:
				}
			}
			close(release)
			body, err := io.ReadAll(reader)
			if err != nil || !strings.Contains(string(body), "response.completed") || !strings.Contains(string(body), "[DONE]") {
				t.Fatalf("terminal lost %s %v", body, err)
			}
			if !expired {
				if err := <-stopped; err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
