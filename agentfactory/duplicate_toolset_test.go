package agentfactory

import (
	"sort"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool/mcptoolset"

	"github.com/normahq/runtime/v2/agentconfig"
)

// recordTransportEndpoints swaps the transport factory for one that records the
// endpoint of every config that actually reaches transport construction, so a
// test asserts on what hostedToolsets did rather than re-deriving the winner.
func recordTransportEndpoints(t *testing.T) *[]string {
	t.Helper()
	original := mcpTransportFactory
	t.Cleanup(func() { mcpTransportFactory = original })

	var endpoints []string
	mcpTransportFactory = func(cfg agentconfig.MCPServerConfig) (mcp.Transport, error) {
		endpoints = append(endpoints, cfg.URL)
		return original(cfg)
	}
	return &endpoints
}

// TestHostedToolsetsCollapsesScopedEndpointDuplicate covers the regression that
// aborted every tool-using turn with `duplicate tool: "balda.control.shutdown"`.
//
// Balda registers the bundled MCP server twice and marks both entries with the
// same DedupKey:
//   - "balda"                    -> http://host:port/mcp/balda
//   - "balda-session-memory-xxx" -> http://host:port/mcp/balda?balda_context=<token>
//
// Only one may reach ADK's flat tool map. The scoped entry must survive: the
// broker reads the context token from the query string to inject the session
// headers, and session-memory calls fail closed without them. It is marked
// DedupPreferred to say so explicitly.
//
// The assertion below inspects the transport the production code actually
// built, so it fails if the survivor changes rather than agreeing with a copy
// of the selection rule.
func TestHostedToolsetsCollapsesScopedEndpointDuplicate(t *testing.T) {
	const endpoint = "http://127.0.0.1:34237/mcp/balda"
	const scopedEndpoint = endpoint + "?balda_context=abc123"
	resolved := map[string]agentconfig.MCPServerConfig{
		"balda": {
			Type:     agentconfig.MCPServerTypeHTTP,
			URL:      endpoint,
			DedupKey: endpoint,
		},
		"balda-session-memory-abc123": {
			Type:           agentconfig.MCPServerTypeHTTP,
			URL:            scopedEndpoint,
			DedupKey:       endpoint,
			DedupPreferred: true,
		},
	}

	endpoints := recordTransportEndpoints(t)

	toolsets, _, err := hostedToolsets(nil, resolved)
	if err != nil {
		t.Fatalf("hostedToolsets: %v", err)
	}
	if got := len(toolsets); got != 1 {
		for i, ts := range toolsets {
			t.Logf("toolset[%d] = %q", i, ts.Name())
		}
		t.Fatalf("expected 1 toolset after collapsing the shared dedup key, got %d", got)
	}

	// The scoped entry must be the one that reaches transport construction.
	if len(*endpoints) != 1 {
		t.Fatalf("expected exactly 1 transport to be built, got %d: %v", len(*endpoints), *endpoints)
	}
	if got := (*endpoints)[0]; got != scopedEndpoint {
		t.Fatalf("survivor endpoint = %q, want the scoped %q", got, scopedEndpoint)
	}
}

// TestDistinctQueryParamsNeverCollapse guards the access-boundary bug the
// reviewer raised: query parameters can select a tenant, so endpoints that
// differ only by query string are distinct servers unless a caller says
// otherwise.
func TestDistinctQueryParamsNeverCollapse(t *testing.T) {
	resolved := map[string]agentconfig.MCPServerConfig{
		"tenant-a": {
			Type: agentconfig.MCPServerTypeHTTP,
			URL:  "http://h:1/mcp?tenant=a",
		},
		"tenant-b": {
			Type: agentconfig.MCPServerTypeHTTP,
			URL:  "http://h:1/mcp?tenant=b",
		},
	}

	endpoints := recordTransportEndpoints(t)

	toolsets, _, err := hostedToolsets(nil, resolved)
	if err != nil {
		t.Fatalf("hostedToolsets: %v", err)
	}
	if got := len(toolsets); got != 2 {
		t.Fatalf("expected 2 toolsets for distinct tenants, got %d", got)
	}
	sort.Strings(*endpoints)
	want := []string{"http://h:1/mcp?tenant=a", "http://h:1/mcp?tenant=b"}
	if len(*endpoints) != len(want) {
		t.Fatalf("expected both tenant endpoints to be built, got %v", *endpoints)
	}
	for i := range want {
		if (*endpoints)[i] != want[i] {
			t.Fatalf("endpoint[%d] = %q, want %q", i, (*endpoints)[i], want[i])
		}
	}
}

// TestDistinctHeadersNeverCollapse covers the second half of the same report:
// the same URL with different Authorization headers can address a different
// authorization context, so headers must not be ignored.
func TestDistinctHeadersNeverCollapse(t *testing.T) {
	resolved := map[string]agentconfig.MCPServerConfig{
		"auth-a": {
			Type:    agentconfig.MCPServerTypeHTTP,
			URL:     "http://h:1/mcp",
			Headers: map[string]string{"Authorization": "Bearer a"},
		},
		"auth-b": {
			Type:    agentconfig.MCPServerTypeHTTP,
			URL:     "http://h:1/mcp",
			Headers: map[string]string{"Authorization": "Bearer b"},
		},
	}

	endpoints := recordTransportEndpoints(t)

	toolsets, _, err := hostedToolsets(nil, resolved)
	if err != nil {
		t.Fatalf("hostedToolsets: %v", err)
	}
	if got := len(toolsets); got != 2 {
		t.Fatalf("expected 2 toolsets for distinct Authorization headers, got %d", got)
	}
	if got := len(*endpoints); got != 2 {
		t.Fatalf("expected both auth configs to be built, got %d", got)
	}
}

// TestSameDedupKeyCollapsesThreeEntries confirms a group of any size collapses
// to exactly one survivor and that the survivor is deterministic.
func TestSameDedupKeyCollapsesThreeEntries(t *testing.T) {
	resolved := map[string]agentconfig.MCPServerConfig{
		"balda-z":        {Type: agentconfig.MCPServerTypeHTTP, URL: "http://h:1/mcp", DedupKey: "bundled"},
		"balda-b":        {Type: agentconfig.MCPServerTypeHTTP, URL: "http://h:1/mcp", DedupKey: "bundled"},
		"balda-a":        {Type: agentconfig.MCPServerTypeHTTP, URL: "http://h:1/mcp", DedupKey: "bundled"},
		"unrelated-http": {Type: agentconfig.MCPServerTypeHTTP, URL: "http://other:9/mcp"},
	}

	endpoints := recordTransportEndpoints(t)

	toolsets, _, err := hostedToolsets(nil, resolved)
	if err != nil {
		t.Fatalf("hostedToolsets: %v", err)
	}
	if got := len(toolsets); got != 2 {
		t.Fatalf("expected 2 toolsets (1 deduped group + 1 unrelated), got %d", got)
	}
	sort.Strings(*endpoints)
	want := []string{"http://h:1/mcp", "http://other:9/mcp"}
	if len(*endpoints) != len(want) {
		t.Fatalf("endpoints = %v, want %v", *endpoints, want)
	}
	// The deduped group contributed exactly one transport.
	seen := map[string]int{}
	for _, e := range *endpoints {
		seen[e]++
	}
	if seen["http://h:1/mcp"] != 1 {
		t.Fatalf("deduped group must build exactly 1 transport, got %d", seen["http://h:1/mcp"])
	}
}

// TestDedupPreferredDecidesPlaceholderWinner pins the rule Balda depends on: a
// marked entry beats an unmarked one, regardless of id order.
func TestDedupPreferredDecidesPlaceholderWinner(t *testing.T) {
	const key = "bundled"
	resolved := map[string]agentconfig.MCPServerConfig{
		// "aaa" sorts first, so without DedupPreferred it would win.
		"aaa-plain": {
			Type:     agentconfig.MCPServerTypeHTTP,
			URL:      "http://h:1/mcp",
			DedupKey: key,
		},
		"zzz-scoped": {
			Type:           agentconfig.MCPServerTypeHTTP,
			URL:            "http://h:1/mcp?balda_context=tok",
			DedupKey:       key,
			DedupPreferred: true,
		},
	}

	endpoints := recordTransportEndpoints(t)

	toolsets, _, err := hostedToolsets(nil, resolved)
	if err != nil {
		t.Fatalf("hostedToolsets: %v", err)
	}
	if got := len(toolsets); got != 1 {
		t.Fatalf("expected 1 toolset, got %d", got)
	}
	if got := (*endpoints)[0]; got != "http://h:1/mcp?balda_context=tok" {
		t.Fatalf("DedupPreferred must win over id ordering, got %q", got)
	}
}

// TestDedupWithoutPreferenceIsLexicographicallySmallest keeps the deterministic
// fallback honest: when nothing is marked, order alone decides.
func TestDedupWithoutPreferenceIsLexicographicallySmallest(t *testing.T) {
	const key = "bundled"
	resolved := map[string]agentconfig.MCPServerConfig{
		"zzz": {Type: agentconfig.MCPServerTypeHTTP, URL: "http://h:1/mcp?z=1", DedupKey: key},
		"aaa": {Type: agentconfig.MCPServerTypeHTTP, URL: "http://h:1/mcp?a=1", DedupKey: key},
		"mmm": {Type: agentconfig.MCPServerTypeHTTP, URL: "http://h:1/mcp?m=1", DedupKey: key},
	}

	endpoints := recordTransportEndpoints(t)

	toolsets, _, err := hostedToolsets(nil, resolved)
	if err != nil {
		t.Fatalf("hostedToolsets: %v", err)
	}
	if got := len(toolsets); got != 1 {
		t.Fatalf("expected 1 toolset, got %d", got)
	}
	if got := (*endpoints)[0]; got != "http://h:1/mcp?a=1" {
		t.Fatalf("expected the lexicographically smallest id to survive, got %q", got)
	}
}

// TestEmptyDedupKeyNeverCollapses pins the safety property: without an explicit
// key the runtime infers nothing, so even byte-identical URLs stay separate.
func TestEmptyDedupKeyNeverCollapses(t *testing.T) {
	resolved := map[string]agentconfig.MCPServerConfig{
		"first":  {Type: agentconfig.MCPServerTypeHTTP, URL: "http://h:1/mcp"},
		"second": {Type: agentconfig.MCPServerTypeHTTP, URL: "http://h:1/mcp"},
	}

	endpoints := recordTransportEndpoints(t)

	toolsets, _, err := hostedToolsets(nil, resolved)
	if err != nil {
		t.Fatalf("hostedToolsets: %v", err)
	}
	if got := len(toolsets); got != 2 {
		t.Fatalf("expected 2 toolsets without a dedup key, got %d", got)
	}
	if got := len(*endpoints); got != 2 {
		t.Fatalf("expected both configs to be built, got %d", got)
	}
}

// TestDedupKeyIsOpaque ensures the runtime does not normalize caller-defined
// keys. Normalizing whitespace would make independently configured namespaces
// collide and could recreate the cross-tenant merge this opt-in API prevents.
func TestDedupKeyIsOpaque(t *testing.T) {
	resolved := map[string]agentconfig.MCPServerConfig{
		"exact": {
			Type:     agentconfig.MCPServerTypeHTTP,
			URL:      "http://h:1/mcp?tenant=exact",
			DedupKey: "tenant-a",
		},
		"spaced": {
			Type:     agentconfig.MCPServerTypeHTTP,
			URL:      "http://h:1/mcp?tenant=spaced",
			DedupKey: " tenant-a ",
		},
	}

	endpoints := recordTransportEndpoints(t)
	toolsets, _, err := hostedToolsets(nil, resolved)
	if err != nil {
		t.Fatalf("hostedToolsets: %v", err)
	}
	if got := len(toolsets); got != 2 {
		t.Fatalf("expected distinct opaque keys to keep 2 toolsets, got %d", got)
	}
	if got := len(*endpoints); got != 2 {
		t.Fatalf("expected 2 transport constructions, got %d", got)
	}
}

// TestHostedToolsetsRejectsMultipleDedupPreferredConfigs makes an ambiguous
// configuration fail before any transport is constructed. Silently selecting
// one would make the active authorization or session context depend on ID
// ordering.
func TestHostedToolsetsRejectsMultipleDedupPreferredConfigs(t *testing.T) {
	resolved := map[string]agentconfig.MCPServerConfig{
		"first": {
			Type:           agentconfig.MCPServerTypeHTTP,
			URL:            "http://h:1/mcp?ctx=first",
			DedupKey:       "bundled",
			DedupPreferred: true,
		},
		"second": {
			Type:           agentconfig.MCPServerTypeHTTP,
			URL:            "http://h:1/mcp?ctx=second",
			DedupKey:       "bundled",
			DedupPreferred: true,
		},
	}

	if _, _, err := hostedToolsets(nil, resolved); err == nil {
		t.Fatal("expected multiple preferred configs to be rejected")
	}
}

func TestHydrateMCPServerConfigPreservesDedupMetadata(t *testing.T) {
	cfg := agentconfig.MCPServerConfig{
		Type:           agentconfig.MCPServerTypeHTTP,
		URL:            "http://h:1/mcp?balda_context=token",
		DedupKey:       "bundled",
		DedupPreferred: true,
	}

	hydrated := hydrateMCPServerConfig(cfg)
	if hydrated.DedupKey != cfg.DedupKey {
		t.Fatalf("DedupKey = %q, want %q", hydrated.DedupKey, cfg.DedupKey)
	}
	if !hydrated.DedupPreferred {
		t.Fatal("DedupPreferred was lost while hydrating the MCP config")
	}
}

// TestHostedToolsetsKeepsDistinctEndpoints guards against over-collapsing:
// genuinely different MCP servers must all survive.
func TestHostedToolsetsKeepsDistinctEndpoints(t *testing.T) {
	resolved := map[string]agentconfig.MCPServerConfig{
		"balda": {
			Type: agentconfig.MCPServerTypeHTTP,
			URL:  "http://127.0.0.1:34237/mcp/balda",
		},
		"executor-mcp": {
			Type: agentconfig.MCPServerTypeHTTP,
			URL:  "http://balda-executor-mcp:8890/mcp",
		},
		"obsidian": {
			Type: agentconfig.MCPServerTypeHTTP,
			URL:  "http://obsidian-mcp:3101/sse",
		},
	}

	toolsets, _, err := hostedToolsets(nil, resolved)
	if err != nil {
		t.Fatalf("hostedToolsets: %v", err)
	}
	if got := len(toolsets); got != 3 {
		t.Fatalf("expected 3 distinct toolsets, got %d", got)
	}
}

// TestMcpToolsetRequestProcessorSignature records the fact that made the
// original diagnosis slow: MCP toolsets are packed by toolPreprocess (via
// Tools()), not by toolsetPreprocess, because the toolset exposes no
// ProcessRequest.
//
// The assertion must use the production signature. A local interface declared
// with `any` parameters is never satisfied by the real method, because Go
// requires exact parameter types, so such a check can never fail and proves
// nothing.
func TestMcpToolsetRequestProcessorSignature(t *testing.T) {
	ts, err := mcptoolset.New(mcptoolset.Config{
		Transport: &mcp.StreamableClientTransport{Endpoint: "http://127.0.0.1:1/mcp"},
	})
	if err != nil {
		t.Fatalf("mcptoolset.New: %v", err)
	}

	if _, ok := any(ts).(requestProcessor); ok {
		t.Fatalf("MCP toolset unexpectedly implements ProcessRequest; dedup premise changed")
	}
}

// requestProcessor mirrors the production request-processor contract from
// google.golang.org/adk/v2/internal/toolinternal. The parameter types must stay
// exact: replacing them with `any` makes the assertion above vacuous.
type requestProcessor interface {
	ProcessRequest(ctx agent.Context, req *model.LLMRequest) error
}

// TestRequestProcessorInterfaceIsSatisfiable is the positive control for the
// assertion above. If the production signature ever changes, this fails instead
// of the negative assertion silently passing forever.
func TestRequestProcessorInterfaceIsSatisfiable(t *testing.T) {
	var _ requestProcessor = (*stubRequestProcessor)(nil)
}

type stubRequestProcessor struct{}

func (*stubRequestProcessor) ProcessRequest(ctx agent.Context, req *model.LLMRequest) error {
	return nil
}
