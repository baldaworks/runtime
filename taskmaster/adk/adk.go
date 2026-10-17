package adk

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	taskmaster "github.com/baldaworks/runtime/v2/taskmaster"
	"github.com/rs/zerolog"
	"google.golang.org/adk/v2/agent"
	adkrunner "google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// Config configures an ADK-backed taskmaster node.
type Config struct {
	AppName      string
	UserID       string
	SessionState map[string]any
	Logger       zerolog.Logger
}

type runner struct {
	mu             sync.Mutex
	inner          agent.Agent
	closer         io.Closer
	runner         *adkrunner.Runner
	sessionService session.Service
	appName        string
	userID         string
	logger         zerolog.Logger
	sessions       map[string]string
	sessionState   map[string]any
}

// Wrap adapts an ADK agent to the taskmaster Node interface.
func Wrap(inner agent.Agent, cfg Config) (taskmaster.Node, error) {
	if inner == nil {
		return nil, errors.New("agent is required")
	}
	appName := strings.TrimSpace(cfg.AppName)
	if appName == "" {
		return nil, errors.New("app name is required")
	}
	userID := strings.TrimSpace(cfg.UserID)
	if userID == "" {
		return nil, errors.New("user id is required")
	}

	sessionService := session.InMemoryService()
	adkRuntime, err := adkrunner.New(adkrunner.Config{
		AppName:        appName,
		Agent:          inner,
		SessionService: sessionService,
	})
	if err != nil {
		if closer, ok := inner.(io.Closer); ok {
			_ = closer.Close()
		}
		return nil, fmt.Errorf("create adk runner: %w", err)
	}

	result := &runner{
		inner:          inner,
		runner:         adkRuntime,
		sessionService: sessionService,
		appName:        appName,
		userID:         userID,
		logger:         cfg.Logger,
		sessions:       make(map[string]string),
		sessionState:   cloneSessionState(cfg.SessionState),
	}
	if closer, ok := inner.(io.Closer); ok {
		result.closer = closer
	}
	return result, nil
}

func (r *runner) Run(ctx context.Context, msg taskmaster.Message, _ taskmaster.EmitFunc) taskmaster.Outcome {
	r.mu.Lock()
	defer r.mu.Unlock()
	resolvedSessionID, err := r.ensureSessionLocked(ctx, msg.SessionID)
	if err != nil {
		return taskmaster.Outcome{Status: taskmaster.OutcomeStatusFailed, Err: err}
	}
	callLogger := r.logger.With().Str("session_id", msg.SessionID).Logger()
	_, last, err := runWithRunner(ctx, r.runner, r.sessionService, r.appName, r.userID, resolvedSessionID, msg.Content, func(output string) {
		callLogger.Debug().Str("output", output).Msg("task output")
	})
	if err != nil {
		return taskmaster.Outcome{Status: taskmaster.OutcomeStatusFailed, Err: err}
	}
	return taskmaster.Outcome{Status: taskmaster.OutcomeStatusCompleted, Content: last}
}

func (r *runner) ensureSessionLocked(ctx context.Context, sessionID string) (string, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return "", errors.New("session_id is required")
	}
	if resolved, ok := r.sessions[sessionID]; ok {
		return resolved, nil
	}
	if _, err := r.sessionService.Get(ctx, &session.GetRequest{
		AppName:   r.appName,
		UserID:    r.userID,
		SessionID: sessionID,
	}); err == nil {
		r.sessions[sessionID] = sessionID
		return sessionID, nil
	}
	created, err := r.sessionService.Create(ctx, &session.CreateRequest{
		AppName:   r.appName,
		UserID:    r.userID,
		SessionID: sessionID,
		State:     cloneSessionState(r.sessionState),
	})
	if err != nil {
		return "", err
	}
	r.sessions[sessionID] = created.Session.ID()
	return created.Session.ID(), nil
}

func (r *runner) Close() error {
	if r.closer == nil {
		return nil
	}
	return r.closer.Close()
}

func runWithRunner(
	ctx context.Context,
	runner *adkrunner.Runner,
	sessionService session.Service,
	appName string,
	userID string,
	sessionID string,
	prompt string,
	onOutput func(string),
) (session.Session, string, error) {
	var lastContent *genai.Content
	events := runner.Run(ctx, userID, sessionID, genai.NewContentFromText(prompt, genai.RoleUser), agent.RunConfig{})
	for ev, runErr := range events {
		if runErr != nil {
			return nil, "", runErr
		}
		if ev != nil && ev.Content != nil {
			lastContent = ev.Content
			output := contentText(ev.Content)
			if onOutput != nil && output != "" {
				onOutput(output)
			}
		}
	}
	finalSession, err := sessionService.Get(ctx, &session.GetRequest{
		AppName:   appName,
		UserID:    userID,
		SessionID: sessionID,
	})
	if err != nil {
		return nil, "", err
	}
	return finalSession.Session, contentText(lastContent), nil
}

func contentText(content *genai.Content) string {
	if content == nil {
		return ""
	}
	var parts []string
	for _, part := range content.Parts {
		if part != nil && strings.TrimSpace(part.Text) != "" {
			parts = append(parts, strings.TrimSpace(part.Text))
		}
	}
	return strings.Join(parts, "\n\n")
}

func cloneSessionState(state map[string]any) map[string]any {
	if len(state) == 0 {
		return nil
	}
	cloned := make(map[string]any, len(state))
	for key, value := range state {
		cloned[key] = value
	}
	return cloned
}
