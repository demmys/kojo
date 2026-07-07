package agent

import (
	"errors"
	"fmt"
)

// Sentinel errors for the agent package.
var (
	ErrAgentNotFound  = errors.New("agent not found")
	ErrAgentBusy      = errors.New("agent is busy")

	// ErrQuiescing is returned by the chat/mutation admission gates
	// (acquirePreparing / AcquireMutation / SetSwitching / acquireResetGuard)
	// while a daemon-wide restart drain is quiescing the manager. It wraps
	// ErrAgentBusy so existing errors.Is(err, ErrAgentBusy) checks (and the
	// HTTP 409 mapping) keep working, while errors.Is(err, ErrQuiescing) lets
	// callers distinguish "server is restarting" from a plain busy agent and
	// surface the dedicated "quiescing" error code.
	ErrQuiescing = fmt.Errorf("%w: server is restarting", ErrAgentBusy)
	ErrAgentResetting = errors.New("agent is being reset")
	ErrAgentArchived  = errors.New("agent is archived")

	ErrGroupNotFound      = errors.New("group not found")
	ErrGroupNotMember     = errors.New("agent is not a member of group")
	ErrGroupTooFew        = errors.New("group requires at least 2 members")
	ErrGroupAlreadyMember = errors.New("agent is already a member of group")

	ErrCredentialNotFound = errors.New("credential not found")
	ErrNoTOTPSecret       = errors.New("no TOTP secret configured")
	ErrInvalidTOTP        = errors.New("invalid TOTP parameters")

	ErrUnsupportedTool    = errors.New("unsupported tool")
	ErrInvalidCronExpr    = errors.New("invalid cron expression")
	ErrUnsupportedTimeout = errors.New("unsupported timeout")

	ErrInvalidRegenerate = errors.New("invalid regenerate target")

	// ErrAgentNotBusy is returned by Steer / SteerOneShot when there is no
	// turn currently running to steer.
	ErrAgentNotBusy = errors.New("agent has no turn in progress")

	// ErrQuestionNotFound is returned by Manager.AnswerQuestion when the
	// given requestID does not match any pending user_question on the
	// agent's running turn (already answered, expired, or never existed).
	ErrQuestionNotFound = errors.New("no pending question with that request id")
)
