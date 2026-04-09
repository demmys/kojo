package slackbot

import (
	"context"
	"log/slog"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
)

// AgentDataDirFunc resolves an agent ID to its data directory path.
type AgentDataDirFunc func(agentID string) string

// hubCommand represents a serialized operation on the Hub's event loop.
type hubCommand struct {
	kind    hubCmdKind
	agentID string
	cfg     agent.SlackBotConfig
	result  chan<- bool // for IsRunning; nil for fire-and-forget commands
}

type hubCmdKind int

const (
	cmdStartBot hubCmdKind = iota
	cmdStopBot
	cmdReconfigure
	cmdIsRunning
	cmdStop
)

// Hub manages all SlackBot instances across agents.
// All operations are serialized through a single event loop goroutine,
// eliminating lock juggling and TOCTOU races.
type Hub struct {
	cmdCh        chan hubCommand
	mgr          ChatManager
	tokens       TokenProvider
	agentDataDir AgentDataDirFunc
	logger       *slog.Logger
	ctx          context.Context
	cancel       context.CancelFunc
	done         chan struct{}
}

// NewHub creates a new Hub and starts its event loop. Call Stop() on shutdown.
// agentDataDir resolves an agent ID to its data directory path for history file storage.
func NewHub(mgr ChatManager, tokens TokenProvider, agentDataDir AgentDataDirFunc, logger *slog.Logger) *Hub {
	ctx, cancel := context.WithCancel(context.Background())
	h := &Hub{
		cmdCh:        make(chan hubCommand, 32),
		mgr:          mgr,
		tokens:       tokens,
		agentDataDir: agentDataDir,
		logger:       logger.With("component", "slackbot-hub"),
		ctx:          ctx,
		cancel:       cancel,
		done:         make(chan struct{}),
	}
	go h.loop()
	return h
}

// loop is the single event loop goroutine that processes all Hub commands.
// Because only this goroutine touches the bots map, no mutex is needed.
func (h *Hub) loop() {
	defer close(h.done)
	bots := make(map[string]*Bot) // agentID → bot

	for cmd := range h.cmdCh {
		switch cmd.kind {
		case cmdStartBot:
			h.doStartBot(bots, cmd.agentID, cmd.cfg)

		case cmdStopBot:
			h.doStopBot(bots, cmd.agentID)

		case cmdReconfigure:
			if !cmd.cfg.Enabled {
				h.doStopBot(bots, cmd.agentID)
			} else {
				h.doStartBot(bots, cmd.agentID, cmd.cfg)
			}

		case cmdIsRunning:
			_, ok := bots[cmd.agentID]
			cmd.result <- ok

		case cmdStop:
			// All bot contexts are children of Hub's context, which was
			// cancelled before this command was sent. Bots are already
			// shutting down concurrently — just wait for each to finish.
			const stopTimeout = 10 * time.Second
			deadline := time.After(stopTimeout)
			for id, bot := range bots {
				select {
				case <-bot.Done():
					// clean exit
				case <-deadline:
					h.logger.Warn("slack bot did not stop within timeout, abandoning", "agent", id)
				}
				delete(bots, id)
			}
			h.logger.Info("all slack bots stopped")
			return // exits the loop; cmdCh will be drained by callers getting zero values
		}
	}
}

func (h *Hub) doStartBot(bots map[string]*Bot, agentID string, cfg agent.SlackBotConfig) {
	if h.tokens == nil {
		h.logger.Warn("no credential store, cannot start slack bot", "agent", agentID)
		return
	}

	appToken, botToken, err := LoadTokens(h.tokens, agentID)
	if err != nil {
		h.logger.Warn("failed to load slack tokens", "agent", agentID, "err", err)
		return
	}
	if appToken == "" || botToken == "" {
		h.logger.Warn("slack tokens not configured", "agent", agentID)
		return
	}

	// Stop existing bot first (synchronous — safe because we're in the single loop goroutine)
	if old, ok := bots[agentID]; ok {
		old.Stop()
		delete(bots, agentID)
	}

	dataDir := ""
	if h.agentDataDir != nil {
		dataDir = h.agentDataDir(agentID)
	}
	bot := NewBot(h.ctx, agentID, dataDir, cfg, appToken, botToken, h.mgr, h.logger)
	bots[agentID] = bot

	go bot.Run()
	h.logger.Info("slack bot started", "agent", agentID)
}

func (h *Hub) doStopBot(bots map[string]*Bot, agentID string) {
	bot, ok := bots[agentID]
	if !ok {
		return
	}
	delete(bots, agentID)
	bot.Stop()
	h.logger.Info("slack bot stopped", "agent", agentID)
}

// send enqueues a command. Returns false if the hub is already stopped.
func (h *Hub) send(cmd hubCommand) bool {
	select {
	case h.cmdCh <- cmd:
		return true
	case <-h.done:
		return false
	}
}

// StartBot starts a Slack bot for the given agent. If one is already running,
// it is stopped first.
func (h *Hub) StartBot(agentID string, cfg agent.SlackBotConfig) {
	h.send(hubCommand{kind: cmdStartBot, agentID: agentID, cfg: cfg})
}

// StopBot stops the Slack bot for the given agent.
func (h *Hub) StopBot(agentID string) {
	h.send(hubCommand{kind: cmdStopBot, agentID: agentID})
}

// Reconfigure stops and restarts the bot with new configuration.
// If the config is disabled, it only stops the bot.
func (h *Hub) Reconfigure(agentID string, cfg agent.SlackBotConfig) {
	h.send(hubCommand{kind: cmdReconfigure, agentID: agentID, cfg: cfg})
}

// Stop stops all bots and shuts down the event loop. Blocks until complete.
func (h *Hub) Stop() {
	h.cancel()
	h.send(hubCommand{kind: cmdStop})
	<-h.done
}

// IsRunning returns true if a bot is running for the given agent.
func (h *Hub) IsRunning(agentID string) bool {
	ch := make(chan bool, 1)
	if !h.send(hubCommand{kind: cmdIsRunning, agentID: agentID, result: ch}) {
		return false
	}
	return <-ch
}
