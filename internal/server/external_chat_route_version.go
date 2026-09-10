package server

import (
	"context"
	"errors"

	"github.com/loppo-llc/kojo/internal/store"
)

type externalChatRouteHint struct {
	Holder  string
	Version store.AgentLockVersion
}

type externalChatRouteVersionKey struct{}
type externalChatRouteVersion struct {
	AgentID string
	Version store.AgentLockVersion
}

var errExternalChatRouteChanged = errors.New("agent ownership changed before external-chat admission; retry on the current holder")

// Capture before discovery or POST, not when its response arrives. A late
// response may finish its admitted turn but cannot teach a newer generation
// to route back to a holder that was valid before a local reclaim/handoff.
func (r *externalChatRouter) withRouteVersion(ctx context.Context, agentID string) (context.Context, error) {
	if captured, ok := ctx.Value(externalChatRouteVersionKey{}).(externalChatRouteVersion); ok && captured.AgentID == agentID {
		return ctx, nil
	}
	v, err := r.currentRouteVersion(ctx, agentID)
	if err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, externalChatRouteVersionKey{}, externalChatRouteVersion{AgentID: agentID, Version: v}), nil
}

func (r *externalChatRouter) refreshRouteVersion(ctx context.Context, agentID string) (context.Context, error) {
	v, err := r.currentRouteVersion(ctx, agentID)
	if err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, externalChatRouteVersionKey{}, externalChatRouteVersion{AgentID: agentID, Version: v}), nil
}

func externalChatVersionFailure(err error) externalChatDispatchResult {
	state := externalChatDispatchDone
	if errors.Is(err, errExternalChatRouteChanged) {
		state = externalChatDispatchSwitching
	}
	return externalChatDispatchResult{state: state, err: err}
}

func (r *externalChatRouter) currentRouteVersion(ctx context.Context, agentID string) (store.AgentLockVersion, error) {
	if r.selfPeerID() == "" || r.server.agents.Store() == nil {
		return store.AgentLockVersion{}, nil
	}
	return r.server.agents.Store().GetAgentLockVersion(ctx, agentID)
}

func (r *externalChatRouter) checkRouteVersion(ctx context.Context, agentID string) error {
	captured, ok := ctx.Value(externalChatRouteVersionKey{}).(externalChatRouteVersion)
	if !ok || captured.AgentID != agentID {
		return errExternalChatRouteChanged
	}
	current, err := r.currentRouteVersion(ctx, agentID)
	if err != nil {
		return err
	}
	if current != captured.Version {
		return errExternalChatRouteChanged
	}
	return nil
}

func (r *externalChatRouter) rememberRouteFrom(ctx context.Context, agentID, holder string) {
	captured, ok := ctx.Value(externalChatRouteVersionKey{}).(externalChatRouteVersion)
	if !ok || captured.AgentID != agentID || holder == "" || r.checkRouteVersion(ctx, agentID) != nil {
		return
	}
	r.mu.Lock()
	r.routes[agentID] = externalChatRouteHint{Holder: holder, Version: captured.Version}
	r.mu.Unlock()
}

func (r *externalChatRouter) forgetRouteFrom(ctx context.Context, agentID, holder string) {
	captured, ok := ctx.Value(externalChatRouteVersionKey{}).(externalChatRouteVersion)
	if !ok || captured.AgentID != agentID {
		return
	}
	r.mu.Lock()
	if hint := r.routes[agentID]; hint.Holder == holder && hint.Version == captured.Version {
		delete(r.routes, agentID)
	}
	r.mu.Unlock()
}
