package runtime

import (
	"context"
	"errors"

	"github.com/Tulipskun/ai-engine/sdk"
)

// SessionSelector is the transport-neutral API for selecting a session.
type SessionSelector struct {
	manager *SessionManager
}

func NewSessionSelector(manager *SessionManager) *SessionSelector {
	return &SessionSelector{manager: manager}
}

// Select resolves and validates a session without applying any transport-specific mapping.
func (s *SessionSelector) Select(ctx context.Context, sessionID string) (*sdk.Session, error) {
	if s == nil || s.manager == nil {
		return nil, errors.New("runtime: session selector is not configured")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if sessionID == "" {
		return nil, errors.New("runtime: session ID is required")
	}
	return s.manager.Resolve(ctx, sdk.Input{SessionID: sessionID})
}
