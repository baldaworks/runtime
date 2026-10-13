package agentfactory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/normahq/runtime/v2/agentconfig"
	"github.com/normahq/runtime/v2/mcpregistry"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	runnerpkg "google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/toolutils"
	"google.golang.org/genai"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/v2/tool/mcptoolset"
)

const cancellationMethod = "notifications/cancelled"

func TestMCPDiscoveryBudgetIncludesInitialization(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&msg)
		if msg.Method == cancellationMethod {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer func() { close(release); srv.Close() }()
	ts, err := mcptoolset.New(mcptoolset.Config{Transport: &mcp.StreamableClientTransport{Endpoint: srv.URL}})
	if err != nil {
		t.Fatal(err)
	}
	bounded := newBoundedMCPToolset(ts, 30*time.Millisecond, time.Second)
	caller, cancel := context.WithTimeout(t.Context(), 400*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = bounded.Tools(lifecycleToolContext{ctx: caller})
	if err == nil {
		t.Fatal("hung initialize succeeded")
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Fatalf("discovery ignored own budget: %v", time.Since(start))
	}
	if caller.Err() != nil {
		t.Fatal("discovery consumed whole caller deadline")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("discovery error=%v", err)
	}
}

func TestFactoryAvailabilityIndependentACPConstruction(t *testing.T) {
	configs := map[string]agentconfig.Config{"fixture": {Type: agentconfig.AgentTypeGenericACP, GenericACP: &agentconfig.ACPConfig{Cmd: []string{"/does/not/exist"}}}}
	f := New(configs, mcpregistry.New(nil), WithAvailabilityIndependentMCP())
	built, err := f.Build(t.Context(), BuildRequest{AgentID: "fixture", WorkingDirectory: t.TempDir()})
	if err != nil {
		t.Fatalf("local ACP factory construction: %v", err)
	}
	if err := built.(io.Closer).Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := New(configs, mcpregistry.New(nil)).Build(t.Context(), BuildRequest{AgentID: "fixture", WorkingDirectory: t.TempDir()}); err == nil {
		t.Fatal("default consumer no longer reports launch error")
	}
}

// The server can stall both the RPC and its detached cancellation notification.
// The owner retains exactly one SDK worker until the cancellation tail drains.
func TestMCPCancellationTailRetainsSlotAndBoundsWaiters(t *testing.T) {
	release := make(chan struct{})
	cancellation := make(chan struct{})
	var observed sync.Once
	var requests atomic.Int32
	peer := mcp.NewServer(&mcp.Implementation{Name: "deadline", Version: "1"}, nil)
	addLifecycleTool(peer, nil)
	normal := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return peer }, nil)
	var stalled atomic.Bool
	stalled.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if !stalled.Load() {
			normal.ServeHTTP(w, r)
			return
		}
		var msg struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&msg)
		if msg.Method == cancellationMethod {
			observed.Do(func() { close(cancellation) })
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	owned := &ownedMCPTransport{Transport: &mcp.StreamableClientTransport{Endpoint: srv.URL}}
	ts, err := mcptoolset.New(mcptoolset.Config{Transport: owned})
	if err != nil {
		t.Fatal(err)
	}
	bounded := newBoundedMCPToolset(ts, 35*time.Millisecond, time.Second)
	owned.operations = bounded
	defer func() { _ = owned.Close() }()
	defer close(release)
	caller, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, err := bounded.Tools(lifecycleToolContext{ctx: caller}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("initial deadline=%v", err)
	}
	select {
	case <-cancellation:
	case <-caller.Done():
		t.Fatal("SDK cancellation notification not observed")
	}
	before := requests.Load()
	for range 3 {
		if _, err := bounded.Tools(lifecycleToolContext{ctx: caller}); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("waiting invocation=%v", err)
		}
	}
	if got := requests.Load(); got != before {
		t.Fatalf("waiters started new SDK work: %d -> %d", before, got)
	}
	stalled.Store(false)
	// Let the owner drain. No later invocation is issued until the worker ends.
	release <- struct{}{}
	bounded.mu.Lock()
	done := bounded.pendingDone
	bounded.mu.Unlock()
	select {
	case <-done:
	case <-caller.Done():
		t.Fatal("SDK worker did not drain")
	}
	tools, err := bounded.Tools(lifecycleToolContext{ctx: caller})
	if err != nil || len(tools) != 1 {
		t.Fatalf("later fresh attachment=%v,%v", tools, err)
	}
}

type deadlineRunContext struct {
	agent.ContextMock
	lifecycleToolContext
	actions session.EventActions
	state   session.State
	sess    session.Session
}

func (c *deadlineRunContext) Deadline() (time.Time, bool)          { return c.lifecycleToolContext.Deadline() }
func (c *deadlineRunContext) Done() <-chan struct{}                { return c.lifecycleToolContext.Done() }
func (c *deadlineRunContext) Err() error                           { return c.lifecycleToolContext.Err() }
func (c *deadlineRunContext) Value(key any) any                    { return c.lifecycleToolContext.Value(key) }
func (c *deadlineRunContext) Actions() *session.EventActions       { return &c.actions }
func (c *deadlineRunContext) Session() session.Session             { return c.sess }
func (c *deadlineRunContext) State() session.State                 { return c.state }
func (c *deadlineRunContext) ReadonlyState() session.ReadonlyState { return c.state }
func (c *deadlineRunContext) FunctionCallID() string               { return "fixture-call" }

func newDeadlineRunContext(t *testing.T) *deadlineRunContext {
	t.Helper()
	svc := session.InMemoryService()
	created, err := svc.Create(t.Context(), &session.CreateRequest{AppName: "deadline", UserID: "u"})
	if err != nil {
		t.Fatal(err)
	}
	return &deadlineRunContext{lifecycleToolContext: lifecycleToolContext{ctx: t.Context()}, actions: session.EventActions{StateDelta: map[string]any{}}, state: created.Session.State(), sess: created.Session}
}

type deadlineFixtureTool struct {
	run func(agent.Context) (map[string]any, error)
}

func (*deadlineFixtureTool) Name() string        { return "fixture" }
func (*deadlineFixtureTool) Description() string { return "deadline fixture" }
func (*deadlineFixtureTool) IsLongRunning() bool { return false }
func (*deadlineFixtureTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{Name: "fixture"}
}
func (t *deadlineFixtureTool) ProcessRequest(_ agent.Context, req *model.LLMRequest) error {
	return toolutils.PackTool(req, t)
}
func (t *deadlineFixtureTool) Run(ctx agent.Context, _ any) (map[string]any, error) {
	return t.run(ctx)
}

func TestMCPExpiredOperationCannotMutateInvocation(t *testing.T) {
	release := make(chan struct{})
	owner := newBoundedMCPToolset(nil, time.Second, 25*time.Millisecond)
	inner := &deadlineFixtureTool{run: func(ctx agent.Context) (map[string]any, error) {
		<-ctx.Done()
		<-release
		if err := ctx.State().Set("late", "value"); err != nil {
			return nil, err
		}
		ctx.Actions().StateDelta["late"] = "delta"
		if err := ctx.RequestConfirmation("late approval", nil); err != nil {
			return nil, err
		}
		return map[string]any{"late": true}, nil
	}}
	wrapped := &boundedMCPTool{mcpFunctionTool: inner, owner: owner}
	ctx := newDeadlineRunContext(t)
	if result, err := wrapped.Run(ctx, nil); !errors.Is(err, context.DeadlineExceeded) || result != nil {
		t.Fatalf("expired result=%v,%v", result, err)
	}
	close(release)
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ctx.State().Get("late"); !errors.Is(err, session.ErrStateKeyNotExist) {
		t.Fatalf("late state committed: %v", err)
	}
	if ctx.actions.SkipSummarization || len(ctx.actions.RequestedToolConfirmations) != 0 || len(ctx.actions.StateDelta) != 0 {
		t.Fatalf("late actions committed: %+v", ctx.actions)
	}
}

func TestMCPToolBudgetAndProcessorParity(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "deadline", Version: "1"}, nil)
	var calls atomic.Int32
	server.AddTool(&mcp.Tool{Name: "slow", Description: "slow fixture", InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		calls.Add(1)
		select {
		case <-time.After(60 * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &mcp.CallToolResult{StructuredContent: map[string]any{"answer": "ok"}}, nil
	})
	srv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	defer srv.Close()
	for _, confirmation := range []bool{false, true} {
		t.Run(fmt.Sprint(confirmation), func(t *testing.T) {
			owned := &ownedMCPTransport{Transport: &mcp.StreamableClientTransport{Endpoint: srv.URL}}
			ts, err := mcptoolset.New(mcptoolset.Config{Transport: owned, RequireConfirmation: confirmation})
			if err != nil {
				t.Fatal(err)
			}
			bounded := newBoundedMCPToolset(ts, 30*time.Millisecond, 300*time.Millisecond)
			owned.operations = bounded
			defer func() { _ = owned.Close() }()
			tools, err := bounded.Tools(lifecycleToolContext{ctx: t.Context()})
			if err != nil || len(tools) != 1 {
				t.Fatalf("tools=%v,%v", tools, err)
			}
			function := tools[0].(mcpFunctionTool)
			if function.Name() != "slow" || function.Description() != "slow fixture" || function.Declaration().ResponseJsonSchema == nil {
				t.Fatalf("function capability lost: %#v", function.Declaration())
			}
			ctx := newDeadlineRunContext(t)
			req := &model.LLMRequest{}
			if err := function.ProcessRequest(ctx, req); err != nil {
				t.Fatal(err)
			}
			if req.Tools["slow"] != function {
				t.Fatal("processor packed an unbounded underlying tool")
			}
			before := calls.Load()
			result, err := function.Run(ctx, map[string]any{})
			if confirmation {
				if !errors.Is(err, tool.ErrConfirmationRequired) || len(ctx.actions.RequestedToolConfirmations) != 1 || calls.Load() != before {
					t.Fatalf("confirmation parity=%v,%+v,calls%d", err, ctx.actions, calls.Load())
				}
			} else if err != nil || result["output"].(map[string]any)["answer"] != "ok" || calls.Load() != before+1 {
				t.Fatalf("tool result=%v,%v,calls%d", result, err, calls.Load())
			}
		})
	}
}

func TestMCPDiscoveryBudgetIncludesEveryPage(t *testing.T) {
	peer := mcp.NewServer(&mcp.Implementation{Name: "pages", Version: "1"}, nil)
	normal := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return peer }, nil)
	var pages atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			normal.ServeHTTP(w, r)
			return
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(data))
		var msg struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.Unmarshal(data, &msg)
		if msg.Method != "tools/list" {
			normal.ServeHTTP(w, r)
			return
		}
		pages.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": map[string]any{"tools": []any{}, "nextCursor": "again"}})
	}))
	defer srv.Close()
	owned := &ownedMCPTransport{Transport: &mcp.StreamableClientTransport{Endpoint: srv.URL}}
	ts, err := mcptoolset.New(mcptoolset.Config{Transport: owned})
	if err != nil {
		t.Fatal(err)
	}
	bounded := newBoundedMCPToolset(ts, 40*time.Millisecond, time.Second)
	owned.operations = bounded
	defer func() { _ = owned.Close() }()
	if _, err := bounded.Tools(lifecycleToolContext{ctx: t.Context()}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("paging deadline=%v", err)
	}
	if pages.Load() < 2 {
		t.Fatalf("fixture did not paginate: %d", pages.Load())
	}
	bounded.mu.Lock()
	done := bounded.pendingDone
	bounded.mu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("paging worker did not drain")
	}
	before := pages.Load()
	time.Sleep(50 * time.Millisecond)
	if pages.Load() != before {
		t.Fatal("idle paging retry")
	}
}

func TestMCPToolReconnectHasOwnBudget(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	peer := mcp.NewServer(&mcp.Implementation{Name: "reconnect", Version: "1"}, nil)
	var calls atomic.Int32
	addLifecycleTool(peer, &calls)
	normal := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return peer }, nil)
	var stall atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !stall.Load() {
			normal.ServeHTTP(w, r)
			return
		}
		var msg struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&msg)
		if msg.Method == cancellationMethod {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	owned := &ownedMCPTransport{Transport: &mcp.StreamableClientTransport{Endpoint: srv.URL}}
	ts, err := mcptoolset.New(mcptoolset.Config{Transport: owned})
	if err != nil {
		t.Fatal(err)
	}
	bounded := newBoundedMCPToolset(ts, time.Second, 40*time.Millisecond)
	owned.operations = bounded
	defer func() { _ = owned.Close() }()
	tools, err := bounded.Tools(lifecycleToolContext{ctx: t.Context()})
	if err != nil {
		t.Fatal(err)
	}
	owned.mu.Lock()
	first := owned.connections[0]
	owned.mu.Unlock()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	stall.Store(true)
	start := time.Now()
	if _, err := tools[0].(mcpFunctionTool).Run(newDeadlineRunContext(t), nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("reconnect deadline=%v", err)
	}
	if time.Since(start) > 250*time.Millisecond {
		t.Fatal("tool reconnect consumed an unbounded setup wait")
	}
	if calls.Load() != 0 {
		t.Fatal("expired reconnect replayed a tool")
	}
	stall.Store(false)
}

func TestMCPSetupCompletionPreservesHealthyStdioProcess(t *testing.T) {
	sets, owners, err := hostedToolsets(nil, map[string]agentconfig.MCPServerConfig{"stdio": {Type: agentconfig.MCPServerTypeStdio, Cmd: []string{os.Args[0], "-test.run=^TestMCPLifecycleServer$"}, Env: map[string]string{"RUNTIME_MCP_LIFECYCLE_HELPER": "1"}}}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := closeMCPTransports(owners); err != nil {
			t.Error(err)
		}
	}()
	tools, err := sets[0].Tools(lifecycleToolContext{ctx: t.Context()})
	if err != nil {
		t.Fatal(err)
	}
	// Tools already canceled its short request context. The owned child must
	// still serve the next invocation using a fresh context.
	if _, err := tools[0].(mcpFunctionTool).Run(newDeadlineRunContext(t), nil); err != nil {
		t.Fatalf("persistent child died with setup context: %v", err)
	}
}

func TestFactoryAvailabilityIndependentACPFirstInvocation(t *testing.T) {
	path := t.TempDir() + "/methods"
	command := helperACPCommand(t)
	command = append(command[:1], append([]string{"GO_AGENTFACTORY_METHOD_LOG=" + path}, command[1:]...)...)
	configs := map[string]agentconfig.Config{"fixture": {Type: agentconfig.AgentTypeGenericACP, GenericACP: &agentconfig.ACPConfig{Cmd: command}, MCPServers: []string{"remote"}}}
	registry := mcpregistry.New(map[string]agentconfig.MCPServerConfig{"remote": {Type: agentconfig.MCPServerTypeHTTP, URL: "http://127.0.0.1:1/exact"}})
	built, err := New(configs, registry, WithAvailabilityIndependentMCP()).Build(t.Context(), BuildRequest{AgentID: "fixture", WorkingDirectory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := built.(io.Closer).Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("constructor contacted ACP: %v", err)
	}
	svc := session.InMemoryService()
	runner, err := runnerpkg.New(runnerpkg.Config{AppName: "lazy", Agent: built, SessionService: svc})
	if err != nil {
		t.Fatal(err)
	}
	created, err := svc.Create(t.Context(), &session.CreateRequest{AppName: "lazy", UserID: "u"})
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range runner.Run(t.Context(), "u", created.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}) {
		if err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"method":"initialize"`)) || !bytes.Contains(data, []byte(`"method":"session/new"`)) || !bytes.Contains(data, []byte(`http://127.0.0.1:1/exact`)) {
		t.Fatalf("missing exact first attachment: %s", data)
	}
}

func TestMCPSuccessfulStateChangeRetainsADKEventDelta(t *testing.T) {
	base := newDeadlineRunContext(t)
	actions := session.EventActions{StateDelta: map[string]any{}}
	ctx := agent.NewToolContext(base, "fixture-call", &actions, nil)
	owner := newBoundedMCPToolset(nil, time.Second, time.Second)
	defer func() {
		if err := owner.Close(); err != nil {
			t.Error(err)
		}
	}()
	inner := &deadlineFixtureTool{run: func(ctx agent.Context) (map[string]any, error) { return nil, ctx.State().Set("completed", "value") }}
	wrapped := &boundedMCPTool{mcpFunctionTool: inner, owner: owner}
	if _, err := wrapped.Run(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if got := ctx.Actions().StateDelta["completed"]; got != "value" {
		t.Fatalf("ADK event state delta lost: %v", got)
	}
	if got, err := ctx.State().Get("completed"); err != nil || got != "value" {
		t.Fatalf("live state=%v,%v", got, err)
	}
}

func TestMCPOwnerCloseCancelsPendingSetup(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var first sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&msg)
		if msg.Method == cancellationMethod {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		first.Do(func() { close(started) })
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)
	owned := &ownedMCPTransport{Transport: &mcp.StreamableClientTransport{Endpoint: srv.URL}}
	ts, err := mcptoolset.New(mcptoolset.Config{Transport: owned})
	if err != nil {
		t.Fatal(err)
	}
	bounded := newBoundedMCPToolset(ts, 5*time.Second, time.Second)
	owned.operations = bounded
	result := make(chan error, 1)
	go func() { _, err := bounded.Tools(lifecycleToolContext{ctx: t.Context()}); result <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("setup did not start")
	}
	start := time.Now()
	if err := owned.Close(); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("owner shutdown did not drain cancelled setup")
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("setup after close=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("caller remained blocked after shutdown")
	}
	if _, err := bounded.Tools(lifecycleToolContext{ctx: t.Context()}); !errors.Is(err, context.Canceled) {
		t.Fatalf("stopped owner accepted setup: %v", err)
	}
}
