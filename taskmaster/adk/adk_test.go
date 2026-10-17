package adk

import (
	"context"
	"fmt"
	"iter"
	"testing"

	taskmaster "github.com/baldaworks/runtime/v2/taskmaster"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

func TestWrapReusesSessionByTaskSessionID(t *testing.T) {
	t.Parallel()

	runner := newTestRunner(t)
	defer closeTestNode(t, runner)

	first := runner.Run(context.Background(), testMessage("session-a", "first"), nil)
	if first.Err != nil {
		t.Fatalf("Run(first) error = %v", first.Err)
	}
	second := runner.Run(context.Background(), testMessage("session-a", "second"), nil)
	if second.Err != nil {
		t.Fatalf("Run(second) error = %v", second.Err)
	}

	if first.Content != "echo:first #1" {
		t.Fatalf("first output = %q, want echo:first #1", first.Content)
	}
	if second.Content != "echo:second #2" {
		t.Fatalf("second output = %q, want echo:second #2", second.Content)
	}
}

func TestWrapIsolatesDistinctSessions(t *testing.T) {
	t.Parallel()

	runner := newTestRunner(t)
	defer closeTestNode(t, runner)

	first := runner.Run(context.Background(), testMessage("session-a", "first"), nil)
	if first.Err != nil {
		t.Fatalf("Run(first) error = %v", first.Err)
	}
	second := runner.Run(context.Background(), testMessage("session-b", "second"), nil)
	if second.Err != nil {
		t.Fatalf("Run(second) error = %v", second.Err)
	}

	if first.Content != "echo:first #1" {
		t.Fatalf("first output = %q, want echo:first #1", first.Content)
	}
	if second.Content != "echo:second #1" {
		t.Fatalf("second output = %q, want echo:second #1", second.Content)
	}
}

func TestWrapCloseClosesInnerAgent(t *testing.T) {
	t.Parallel()

	inner, err := newCountingAgent()
	if err != nil {
		t.Fatalf("newCountingAgent() error = %v", err)
	}
	closable := &closableAgent{Agent: inner}
	runner, err := Wrap(closable, Config{
		AppName: "taskmaster-test",
		UserID:  "root",
	})
	if err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	closer, ok := runner.(interface{ Close() error })
	if !ok {
		t.Fatal("Wrap() result does not expose Close")
	}
	if err := closer.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if !closable.closed {
		t.Fatal("Close() did not close the wrapped agent")
	}
}

func newTestRunner(t *testing.T) taskmaster.Node {
	t.Helper()

	inner, err := newCountingAgent()
	if err != nil {
		t.Fatalf("newCountingAgent() error = %v", err)
	}
	runner, err := Wrap(inner, Config{
		AppName:      "taskmaster-test",
		UserID:       "root",
		SessionState: map[string]any{"prefix": "echo:"},
	})
	if err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	return runner
}

func closeTestNode(t *testing.T, node taskmaster.Node) {
	t.Helper()

	closer, ok := node.(interface{ Close() error })
	if ok {
		if err := closer.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}
}

func testMessage(sessionID string, content string) taskmaster.Message {
	return taskmaster.Message{
		SessionID: sessionID,
		Kind:      taskmaster.MessageKindJob,
		From:      taskmaster.NewCLIInputLocator(),
		To:        taskmaster.NewAgentLocator("root"),
		Content:   content,
	}
}

type closableAgent struct {
	agent.Agent
	closed bool
}

func (a *closableAgent) Close() error {
	a.closed = true
	return nil
}

func newCountingAgent() (agent.Agent, error) {
	return agent.New(agent.Config{
		Name:        "counting-agent",
		Description: "Test agent for taskmaster/adk",
		Run: func(ctx agent.InvocationContext) iter.Seq2[*session.Event, error] {
			return func(yield func(*session.Event, error) bool) {
				count := 1
				if value, err := ctx.Session().State().Get("count"); err == nil {
					if existing, ok := value.(int); ok {
						count = existing + 1
					}
				}

				prefix := ""
				if value, err := ctx.Session().State().Get("prefix"); err == nil {
					if existing, ok := value.(string); ok {
						prefix = existing
					}
				}

				text := prefix + userText(ctx.UserContent()) + fmt.Sprintf(" #%d", count)
				yield(&session.Event{
					LLMResponse: model.LLMResponse{
						Content: genai.NewContentFromText(text, genai.RoleModel),
					},
					Actions: session.EventActions{
						StateDelta: map[string]any{"count": count},
					},
				}, nil)
			}
		},
	})
}

func userText(content *genai.Content) string {
	if content == nil {
		return ""
	}
	for _, part := range content.Parts {
		if part != nil && part.Text != "" {
			return part.Text
		}
	}
	return ""
}
