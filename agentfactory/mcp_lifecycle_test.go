package agentfactory

import (
	"context"
	"errors"
	"fmt"
	acpagent "github.com/normahq/go-adk-acpagent/v2"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/normahq/runtime/v2/agentconfig"
	"github.com/normahq/runtime/v2/hostedagent"
	"github.com/normahq/runtime/v2/mcpregistry"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/toolconfirmation"
)

type lifecycleToolContext struct {
	agent.ReadonlyContext
	ctx context.Context
}

func (c lifecycleToolContext) Deadline() (time.Time, bool) { return c.ctx.Deadline() }
func (c lifecycleToolContext) Done() <-chan struct{}       { return c.ctx.Done() }
func (c lifecycleToolContext) Err() error                  { return c.ctx.Err() }
func (c lifecycleToolContext) Value(key any) any           { return c.ctx.Value(key) }

type lifecycleRunContext struct {
	agent.Context
	lifecycleToolContext
}

func (c lifecycleRunContext) Deadline() (time.Time, bool)                        { return c.lifecycleToolContext.Deadline() }
func (c lifecycleRunContext) Done() <-chan struct{}                              { return c.lifecycleToolContext.Done() }
func (c lifecycleRunContext) Err() error                                         { return c.lifecycleToolContext.Err() }
func (c lifecycleRunContext) Value(k any) any                                    { return c.lifecycleToolContext.Value(k) }
func (lifecycleRunContext) ToolConfirmation() *toolconfirmation.ToolConfirmation { return nil }

type lifecycleToolRunner interface {
	Run(ctx agent.Context, args any) (map[string]any, error)
}

func addLifecycleTool(server *mcp.Server, calls *atomic.Int32) {
	server.AddTool(&mcp.Tool{Name: "ping", Description: "fixture", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if calls != nil {
			calls.Add(1)
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	})
}

type callerToolset struct{ closed atomic.Bool }

func (*callerToolset) Name() string                                     { return "caller" }
func (*callerToolset) Tools(agent.ReadonlyContext) ([]tool.Tool, error) { return nil, nil }
func (t *callerToolset) Close() error                                   { t.closed.Store(true); return nil }

func TestHostedMCPLifetime(t *testing.T) {
	const poolProvider = "pool"
	for _, provider := range []string{"openai", "aistudio", poolProvider} {
		t.Run(provider, func(t *testing.T) {
			oldOpenAI, oldStudio, oldHosted, oldTransport := newOpenAIModel, newAIStudioModel, newHostedAgent, mcpTransportFactory
			t.Cleanup(func() {
				newOpenAIModel, newAIStudioModel, newHostedAgent, mcpTransportFactory = oldOpenAI, oldStudio, oldHosted, oldTransport
			})
			newOpenAIModel = func(string, string, time.Duration, string) (model.LLM, error) {
				return fakeHostedModel{name: "fixture"}, nil
			}
			newAIStudioModel = func(context.Context, string, string) (model.LLM, error) { return fakeHostedModel{name: "fixture"}, nil }
			var toolsets []tool.Toolset
			newHostedAgent = func(cfg hostedagent.Config) (agent.Agent, error) {
				toolsets = cfg.Toolsets
				return newHostedAgentDefault(cfg)
			}
			var commands []*exec.Cmd
			mcpTransportFactory = func(cfg agentconfig.MCPServerConfig) (mcp.Transport, error) {
				tr, err := oldTransport(cfg)
				if ct, ok := tr.(*mcp.CommandTransport); ok {
					commands = append(commands, ct.Command)
				}
				return tr, err
			}
			// Cleanup runs only after the exit assertions; it cannot make them pass.
			t.Cleanup(func() {
				for _, cmd := range commands {
					if cmd.Process != nil && cmd.ProcessState == nil {
						_ = cmd.Process.Kill()
					}
				}
			})
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			definitions := map[string]agentconfig.MCPServerConfig{
				"one": {Type: agentconfig.MCPServerTypeStdio, Cmd: []string{executable, "-test.run=^TestMCPLifecycleServer$"}, Env: map[string]string{"RUNTIME_MCP_LIFECYCLE_HELPER": "1"}},
				"two": {Type: agentconfig.MCPServerTypeStdio, Cmd: []string{executable, "-test.run=^TestMCPLifecycleServer$"}, Env: map[string]string{"RUNTIME_MCP_LIFECYCLE_HELPER": "1"}},
			}
			providers := map[string]agentconfig.Config{
				"openai":     {Type: agentconfig.AgentTypeOpenAI, OpenAI: &agentconfig.LocalAPIConfig{APIKey: "fixture", Model: "fixture"}, MCPServers: []string{"one", "two"}},
				"aistudio":   {Type: agentconfig.AgentTypeAIStudio, AIStudio: &agentconfig.LocalAPIConfig{APIKey: "fixture", Model: "fixture"}, MCPServers: []string{"one", "two"}},
				poolProvider: {Type: agentconfig.AgentTypePool, PoolConfig: &agentconfig.PoolConfig{Members: []string{"openai"}}},
			}
			caller := &callerToolset{}
			req := BuildRequest{AgentID: provider, Name: "root", WorkingDirectory: t.TempDir()}
			if provider != poolProvider {
				req.Toolsets = []tool.Toolset{caller}
			}
			ag, err := New(providers, mcpregistry.New(definitions)).Build(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var tools []tool.Tool
			for _, ts := range toolsets {
				got, err := ts.Tools(lifecycleToolContext{ctx: ctx})
				if err != nil {
					t.Fatalf("discover tools: %v", err)
				}
				tools = append(tools, got...)
			}
			if len(commands) != 2 {
				t.Fatalf("started transports = %d, want 2", len(commands))
			}
			for _, cmd := range commands {
				if cmd.Process == nil {
					t.Fatal("MCP was not started")
				}
				t.Logf("MCP pid=%d", cmd.Process.Pid)
			}
			if provider != poolProvider && ag.FindAgent(ag.Name()) != ag {
				t.Fatal("FindAgent lost owner identity")
			}
			if provider != poolProvider {
				if _, ok := ag.(hostedAgentCapabilities); !ok {
					t.Fatal("hosted ADK live/node capabilities lost")
				}
			}
			closer, ok := ag.(io.Closer)
			if !ok {
				t.Fatal("factory-created hosted agent cannot close its live MCP connections")
			}
			if err := closer.Close(); err != nil {
				t.Fatalf("close agent: %v", err)
			}
			if err := closer.Close(); err != nil {
				t.Fatalf("repeat close agent: %v", err)
			}
			for _, cmd := range commands {
				if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
					t.Fatalf("MCP pid=%d was not reaped by owner Close", cmd.Process.Pid)
				}
			}
			if caller.closed.Load() {
				t.Fatal("caller-owned toolset closed by factory")
			}
			for _, tl := range tools {
				runner, ok := tl.(lifecycleToolRunner)
				if !ok {
					t.Fatal("MCP tool has no Run")
				}
				if _, err := runner.Run(lifecycleRunContext{lifecycleToolContext: lifecycleToolContext{ctx: ctx}}, map[string]any{}); err == nil {
					t.Fatal("closed agent reopened its MCP transport")
				}
			}
		})
	}
}

func TestMCPLifecycleServer(t *testing.T) {
	if os.Getenv("RUNTIME_MCP_LIFECYCLE_HELPER") != "1" {
		return
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "lifetime", Version: "1"}, nil)
	addLifecycleTool(server, nil)
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

type lifecycleTransportFunc func(context.Context) (mcp.Connection, error)

func (f lifecycleTransportFunc) Connect(ctx context.Context) (mcp.Connection, error) { return f(ctx) }

type lifecycleConnection struct {
	mcp.Connection
	closes atomic.Int32
	err    error
}

func (c *lifecycleConnection) Close() error { c.closes.Add(1); return c.err }

type versionedLifecycleTransport struct{ lifecycleTransportFunc }

func (versionedLifecycleTransport) SupportsProtocolVersion(v string) bool { return v == "supported" }

func TestMCPConnectRacingClose(t *testing.T) {
	started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	conn := &lifecycleConnection{}
	transport := &ownedMCPTransport{Transport: lifecycleTransportFunc(func(ctx context.Context) (mcp.Connection, error) {
		close(started)
		<-ctx.Done()
		close(canceled)
		<-release
		return conn, nil
	})}
	result := make(chan error, 1)
	go func() {
		got, err := transport.Connect(context.Background())
		if got != nil {
			err = errors.New("late connection escaped closed owner")
		}
		result <- err
	}()
	<-started
	closed := make(chan error, 1)
	go func() { closed <- transport.Close() }()
	<-canceled
	select {
	case err := <-closed:
		t.Fatalf("Close returned before late cleanup: %v", err)
	default:
	}
	close(release)
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, mcp.ErrConnectionClosed) {
		t.Fatalf("late Connect error=%v", err)
	}
	if got := conn.closes.Load(); got != 1 {
		t.Fatalf("late connection closes=%d", got)
	}
	if _, err := transport.Connect(context.Background()); !errors.Is(err, mcp.ErrConnectionClosed) {
		t.Fatalf("Connect after Close error=%v", err)
	}
}

func TestMCPTransportConcurrentClose(t *testing.T) {
	sentinel := errors.New("connection close failed")
	conn := &lifecycleConnection{err: sentinel}
	transport := &ownedMCPTransport{Transport: lifecycleTransportFunc(func(context.Context) (mcp.Connection, error) { return conn, nil })}
	got, err := transport.Connect(context.Background())
	if err != nil || got != conn {
		t.Fatalf("SDK connection identity changed: %v, %v", got, err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if err := transport.Close(); !errors.Is(err, sentinel) {
				t.Errorf("Close error=%v", err)
			}
		})
	}
	wg.Wait()
	if got := conn.closes.Load(); got != 1 {
		t.Fatalf("connection Close calls=%d", got)
	}
}

func TestMCPTransportProtocolCapability(t *testing.T) {
	tr := &ownedMCPTransport{Transport: versionedLifecycleTransport{}}
	if !tr.SupportsProtocolVersion("supported") || tr.SupportsProtocolVersion("other") {
		t.Fatal("wrapped protocol capability changed")
	}
	plain := &ownedMCPTransport{Transport: lifecycleTransportFunc(nil)}
	if !plain.SupportsProtocolVersion("other") {
		t.Fatal("plain transport no longer supports SDK default protocols")
	}
}

func TestHostedMCPConstructorFailureClosesConnections(t *testing.T) {
	originalHosted, originalTransport := newHostedAgent, mcpTransportFactory
	t.Cleanup(func() { newHostedAgent, mcpTransportFactory = originalHosted, originalTransport })
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var cmd *exec.Cmd
	mcpTransportFactory = func(cfg agentconfig.MCPServerConfig) (mcp.Transport, error) {
		tr, err := originalTransport(cfg)
		if ct, ok := tr.(*mcp.CommandTransport); ok {
			cmd = ct.Command
		}
		return tr, err
	}
	t.Cleanup(func() {
		if cmd != nil && cmd.Process != nil && cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
		}
	})
	sentinel := errors.New("constructor failed")
	newHostedAgent = func(cfg hostedagent.Config) (agent.Agent, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, ts := range cfg.Toolsets {
			if _, err := ts.Tools(lifecycleToolContext{ctx: ctx}); err != nil {
				return nil, err
			}
		}
		return nil, sentinel
	}
	_, err = buildHostedAgent(hostedagent.Config{Model: fakeHostedModel{name: "fixture"}}, map[string]agentconfig.MCPServerConfig{"one": {Type: agentconfig.MCPServerTypeStdio, Cmd: []string{executable, "-test.run=^TestMCPLifecycleServer$"}, Env: map[string]string{"RUNTIME_MCP_LIFECYCLE_HELPER": "1"}}})
	if !errors.Is(err, sentinel) {
		t.Fatalf("constructor error=%v", err)
	}
	if cmd == nil || cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
		t.Fatal("failed construction retained live MCP process")
	}
}

func TestMCPFailedAllocationClosesConnection(t *testing.T) {
	sentinel := errors.New("allocation failed")
	conn := &lifecycleConnection{}
	tr := &ownedMCPTransport{Transport: lifecycleTransportFunc(func(context.Context) (mcp.Connection, error) { return conn, sentinel })}
	if got, err := tr.Connect(context.Background()); got != nil || !errors.Is(err, sentinel) {
		t.Fatalf("failed allocation=%v,%v", got, err)
	}
	if got := conn.closes.Load(); got != 1 {
		t.Fatalf("failed allocation closes=%d", got)
	}
}

func TestMCPHTTPCloseDeadline(t *testing.T) {
	release := make(chan struct{})
	server := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			select {
			case <-r.Context().Done():
				return
			case <-release:
			}
		}
		handler.ServeHTTP(w, r)
	}))
	defer srv.Close()
	defer close(release)
	tr, err := mcpTransportForConfig(agentconfig.MCPServerConfig{Type: agentconfig.MCPServerTypeHTTP, URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	conn, err := client.Connect(ctx, tr, nil)
	if err != nil {
		t.Fatal(err)
	}
	if conn.ID() == "" {
		t.Fatal("fixture did not establish stateful MCP session")
	}
	done := make(chan error, 1)
	go func() { done <- conn.Close() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("stalled DELETE closed without a timeout error")
		}
	case <-time.After(7 * time.Second):
		t.Fatal("stalled DELETE exceeded bounded shutdown")
	}
}

func TestMCPConnectLateCleanupError(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	sentinel := errors.New("late close failed")
	conn := &lifecycleConnection{err: sentinel}
	tr := &ownedMCPTransport{Transport: lifecycleTransportFunc(func(ctx context.Context) (mcp.Connection, error) {
		close(started)
		<-ctx.Done()
		<-release
		return conn, nil
	})}
	result := make(chan error, 1)
	go func() { _, err := tr.Connect(context.Background()); result <- err }()
	<-started
	closed := make(chan error, 1)
	go func() { closed <- tr.Close() }()
	close(release)
	if err := <-closed; !errors.Is(err, sentinel) {
		t.Fatalf("late cleanup error lost by Close: %v", err)
	}
	if err := <-result; !errors.Is(err, mcp.ErrConnectionClosed) || !errors.Is(err, sentinel) {
		t.Fatalf("late Connect error=%v", err)
	}
}

func TestMCPConnectShutdownTimeout(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	conn := &lifecycleConnection{}
	tr := &ownedMCPTransport{Transport: lifecycleTransportFunc(func(context.Context) (mcp.Connection, error) { close(started); <-release; return conn, nil })}
	result := make(chan error, 1)
	go func() { _, err := tr.Connect(context.Background()); result <- err }()
	<-started
	if err := tr.Close(); !errors.Is(err, context.DeadlineExceeded) {
		close(release)
		t.Fatalf("uncancellable Connect shutdown error=%v", err)
	}
	close(release)
	if err := <-result; !errors.Is(err, mcp.ErrConnectionClosed) {
		t.Fatalf("late result=%v", err)
	}
	if conn.closes.Load() != 1 {
		t.Fatal("late allocation after timeout was not closed")
	}
}

func TestMCPPoolACPContext(t *testing.T) {
	original := newACPAgent
	t.Cleanup(func() { newACPAgent = original })
	var constructorCtx context.Context
	newACPAgent = func(cfg acpagent.Config) (agent.Agent, error) {
		constructorCtx = cfg.Context //nolint:staticcheck // The current factory supplies this lifecycle context.
		return newACPAgentDefault(cfg)
	}
	t.Setenv("GO_WANT_AGENTFACTORY_ACP_HELPER", "1")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	providers := map[string]agentconfig.Config{
		"member": {Type: agentconfig.AgentTypeGenericACP, GenericACP: &agentconfig.ACPConfig{Cmd: []string{executable, "-test.run=^TestAgentFactoryACPHelperProcess$"}}},
		"pool":   {Type: agentconfig.AgentTypePool, PoolConfig: &agentconfig.PoolConfig{Members: []string{"member"}}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ag, err := New(providers, mcpregistry.New(nil)).Build(ctx, BuildRequest{AgentID: "pool", WorkingDirectory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	closer, ok := ag.(io.Closer)
	if !ok {
		t.Fatal("pool has no Close")
	}
	t.Cleanup(func() { _ = closer.Close() })
	if constructorCtx == nil || constructorCtx.Err() != nil {
		t.Fatal("real pooled ACP constructor context canceled before shutdown")
	}
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	if constructorCtx.Err() != context.Canceled {
		t.Fatal("pooled ACP context not released at shutdown")
	}
}

func TestMCPHeaderlessUsesDefaultClient(t *testing.T) {
	old := http.DefaultClient
	defer func() { http.DefaultClient = old }()
	redirects := 0
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/done", http.StatusFound)
			return
		}
		if c, err := r.Cookie("fixture"); err != nil || c.Value != "default-client" {
			http.Error(w, "missing jar cookie", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	endpoint, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	jar.SetCookies(endpoint, []*http.Cookie{{Name: "fixture", Value: "default-client", Path: "/"}})
	http.DefaultClient = &http.Client{Transport: old.Transport, Jar: jar, Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { redirects++; return nil }}
	response, err := httpClientWithHeaders(nil).Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	if response.StatusCode != http.StatusOK || redirects != 1 {
		t.Fatalf("default client cookies/redirects changed: status=%d redirects=%d", response.StatusCode, redirects)
	}
}

func TestMCPHostedReconnect(t *testing.T) {
	var calls atomic.Int32
	server := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	addLifecycleTool(server, &calls)
	srv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	t.Cleanup(srv.Close)
	original := mcpTransportFactory
	t.Cleanup(func() { mcpTransportFactory = original })
	var connects atomic.Int32
	var firstConnection mcp.Connection
	mcpTransportFactory = func(cfg agentconfig.MCPServerConfig) (mcp.Transport, error) {
		tr, err := original(cfg)
		if err != nil {
			return nil, err
		}
		return lifecycleTransportFunc(func(ctx context.Context) (mcp.Connection, error) {
			n := connects.Add(1)
			conn, err := tr.Connect(ctx)
			if n == 1 {
				firstConnection = conn
			}
			return conn, err
		}), nil
	}
	sets, owners, err := hostedToolsets(nil, map[string]agentconfig.MCPServerConfig{"fixture": {Type: agentconfig.MCPServerTypeHTTP, URL: srv.URL}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeMCPTransports(owners) })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tools, err := sets[0].Tools(lifecycleToolContext{ctx: ctx})
	if err != nil || len(tools) != 1 {
		t.Fatalf("discover tools: %v %v", tools, err)
	}
	runner, ok := tools[0].(lifecycleToolRunner)
	if !ok {
		t.Fatal("MCP tool has no Run")
	}
	runCtx := lifecycleRunContext{lifecycleToolContext: lifecycleToolContext{ctx: ctx}}
	call := func() {
		t.Helper()
		if _, err := runner.Run(runCtx, map[string]any{}); err != nil {
			t.Fatal(err)
		}
	}
	call()
	// Inject connection loss through the public SDK boundary. Newer protocols
	// can keep serving after ServerSession.Close, so that is not transport loss.
	if firstConnection == nil {
		t.Fatal("fixture did not retain initial connection")
	}
	if err := firstConnection.Close(); err != nil {
		t.Fatal(err)
	}

	call()
	if connects.Load() != 2 || calls.Load() != 2 {
		t.Fatalf("real reconnect=%d tool calls=%d, want2/2", connects.Load(), calls.Load())
	}
	if err := closeMCPTransports(owners); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(runCtx, map[string]any{}); err == nil {
		t.Fatal("closed owner executed MCP call")
	}
	if connects.Load() != 2 || calls.Load() != 2 {
		t.Fatal("closed owner allocated/executed another connection/call")
	}
}
