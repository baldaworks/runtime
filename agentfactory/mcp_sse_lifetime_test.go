package agentfactory

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestOwnedSSEConnectionSurvivesSuccessfulSetupCancellation(t *testing.T) {
	peer := mcp.NewServer(&mcp.Implementation{Name: "owned-sse-lifetime", Version: "1"}, nil)
	addLifecycleTool(peer, nil)
	streamEnded := make(chan struct{})
	handler := mcp.NewSSEHandler(func(*http.Request) *mcp.Server { return peer }, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
		if r.Method == http.MethodGet {
			close(streamEnded)
		}
	}))
	defer server.Close()
	owner := &ownedMCPTransport{Transport: &mcp.SSEClientTransport{Endpoint: server.URL}}
	defer func() { _ = owner.Close() }()
	setup, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	client, err := mcp.NewClient(&mcp.Implementation{Name: "setup-client", Version: "1"}, nil).Connect(setup, owner, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListTools(setup, &mcp.ListToolsParams{}); err != nil {
		t.Fatal(err)
	}
	cancel()
	// A completed setup budget must not own the established SSE response body.
	select {
	case <-streamEnded:
		t.Fatal("successful setup cancellation closed the owned SSE connection")
	case <-time.After(100 * time.Millisecond):
	}
	call, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	if _, err := client.CallTool(call, &mcp.CallToolParams{Name: "ping"}); err != nil {
		t.Fatalf("established tool after setup cancellation: %v", err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-streamEnded:
	case <-call.Done():
		t.Fatal("owner Close retained the SSE stream")
	}
}

func TestOwnedSSESetupCancellationReleasesStream(t *testing.T) {
	for _, phase := range []string{"connect", "initialize"} {
		t.Run(phase, func(t *testing.T) {
			entered, ended := make(chan struct{}), make(chan struct{})
			peer := mcp.NewServer(&mcp.Implementation{Name: "canceled-sse", Version: "1"}, nil)

			handler := mcp.NewSSEHandler(func(*http.Request) *mcp.Server { return peer }, nil)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if phase == "initialize" && r.Method == http.MethodPost {
					body, _ := io.ReadAll(r.Body)
					r.Body = io.NopCloser(bytes.NewReader(body))
					var message struct{ Method string }
					_ = json.Unmarshal(body, &message)
					if message.Method == "initialize" || message.Method == "server/discover" {
						close(entered)
						<-r.Context().Done()
						return
					}
				}
				if r.Method == http.MethodGet {
					defer close(ended)
					if phase == "connect" {
						w.Header().Set("Content-Type", "text/event-stream")
						w.(http.Flusher).Flush()
						close(entered)
						<-r.Context().Done()
						return
					}
				}
				handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			owner := &ownedMCPTransport{Transport: &mcp.SSEClientTransport{Endpoint: server.URL}}
			defer func() { _ = owner.Close() }()
			setup, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				_, err := mcp.NewClient(&mcp.Implementation{Name: "canceled-setup", Version: "1"}, nil).Connect(setup, owner, nil)
				result <- err
			}()
			guard, stop := context.WithTimeout(t.Context(), 5*time.Second)
			defer stop()
			select {
			case <-entered:
			case err := <-result:
				t.Fatalf("setup returned before cancellation barrier: %v", err)
			case <-guard.Done():
				t.Fatal("setup did not reach cancellation barrier")
			}
			cancel()
			select {
			case err := <-result:
				if err == nil {
					t.Fatal("canceled setup succeeded")
				}
			case <-guard.Done():
				t.Fatal("canceled setup did not return")
			}
			select {
			case <-ended:
			case <-guard.Done():
				t.Fatal("failed setup retained its SSE stream")
			}
		})
	}
}
