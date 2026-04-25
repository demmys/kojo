package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"

	"github.com/loppo-llc/kojo/internal/configdir"
)

const (
	vapidFile         = "vapid.json"
	subscriptionsFile = "push_subscriptions.json"
)

type Manager struct {
	mu            sync.Mutex
	logger        *slog.Logger
	vapidPrivate  string
	vapidPublic   string
	subscriptions []*webpush.Subscription
	// persistMu serializes writes to the subscriptions file so concurrent
	// Subscribe / Unsubscribe / Send-driven persists cannot race on the
	// shared .tmp filename or commit out-of-order snapshots.
	persistMu sync.Mutex
}

type vapidKeys struct {
	PrivateKey string `json:"privateKey"`
	PublicKey  string `json:"publicKey"`
}

func NewManager(logger *slog.Logger) (*Manager, error) {
	m := &Manager{
		logger:        logger,
		subscriptions: make([]*webpush.Subscription, 0),
	}
	if err := m.loadOrGenerateVAPID(); err != nil {
		return nil, err
	}
	m.loadSubscriptions()
	return m, nil
}

func (m *Manager) VAPIDPublicKey() string {
	return m.vapidPublic
}

func (m *Manager) Subscribe(sub *webpush.Subscription) {
	m.mu.Lock()
	for i, existing := range m.subscriptions {
		if existing.Endpoint == sub.Endpoint {
			m.subscriptions[i] = sub
			m.mu.Unlock()
			m.persistSubscriptions()
			return
		}
	}
	m.subscriptions = append(m.subscriptions, sub)
	m.mu.Unlock()
	ep := sub.Endpoint
	if len(ep) > 50 {
		ep = ep[:50] + "..."
	}
	m.logger.Info("push subscription added", "endpoint", ep)
	m.persistSubscriptions()
}

func (m *Manager) Unsubscribe(endpoint string) {
	m.mu.Lock()
	removed := false
	for i, sub := range m.subscriptions {
		if sub.Endpoint == endpoint {
			m.subscriptions = append(m.subscriptions[:i], m.subscriptions[i+1:]...)
			removed = true
			break
		}
	}
	m.mu.Unlock()
	if removed {
		m.persistSubscriptions()
	}
}

func (m *Manager) Send(payload []byte) {
	m.mu.Lock()
	subs := make([]*webpush.Subscription, len(m.subscriptions))
	copy(subs, m.subscriptions)
	m.mu.Unlock()

	var expired []string

	for _, sub := range subs {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		resp, err := webpush.SendNotificationWithContext(ctx, payload, sub, &webpush.Options{
			VAPIDPublicKey:  m.vapidPublic,
			VAPIDPrivateKey: m.vapidPrivate,
			Subscriber:      "kojo@localhost",
			TTL:             86400, // 24 hours
			Urgency:         webpush.UrgencyHigh,
			// webpush-go pads the encrypted record up to RecordSize bytes.
			// Default (4096) yields a request body that Mozilla autopush rejects with 413.
			// 2048 still gives plenty of length-hiding padding while staying well below
			// every push provider's documented payload cap.
			RecordSize: 2048,
		})
		cancel()
		if err != nil {
			m.logger.Warn("push send failed", "err", err)
			continue
		}
		resp.Body.Close()

		if resp.StatusCode == 410 || resp.StatusCode == 404 {
			expired = append(expired, sub.Endpoint)
			m.logger.Info("push subscription expired, removing", "status", resp.StatusCode)
		} else if resp.StatusCode >= 400 {
			ep := sub.Endpoint
			if len(ep) > 50 {
				ep = ep[:50] + "..."
			}
			m.logger.Warn("push send error", "status", resp.StatusCode, "endpoint", ep)
		}
	}

	// remove expired subscriptions in one pass to avoid N persistence writes
	if len(expired) > 0 {
		expiredSet := make(map[string]struct{}, len(expired))
		for _, ep := range expired {
			expiredSet[ep] = struct{}{}
		}
		m.mu.Lock()
		kept := m.subscriptions[:0]
		for _, s := range m.subscriptions {
			if _, drop := expiredSet[s.Endpoint]; drop {
				continue
			}
			kept = append(kept, s)
		}
		m.subscriptions = kept
		m.mu.Unlock()
		m.persistSubscriptions()
	}
}

func (m *Manager) loadOrGenerateVAPID() error {
	dir := configdir.Path()
	path := filepath.Join(dir, vapidFile)

	data, err := os.ReadFile(path)
	if err == nil {
		var keys vapidKeys
		if jsonErr := json.Unmarshal(data, &keys); jsonErr != nil {
			return fmt.Errorf("corrupted VAPID key file %s: %w", path, jsonErr)
		}
		if keys.PrivateKey == "" || keys.PublicKey == "" {
			return fmt.Errorf("incomplete VAPID key file %s: missing privateKey or publicKey", path)
		}
		m.vapidPrivate = keys.PrivateKey
		m.vapidPublic = keys.PublicKey
		m.logger.Info("loaded VAPID keys")
		return nil
	}

	// generate new keys using webpush-go's GenerateVAPIDKeys
	// which produces raw P-256 scalar (base64url) as expected by the library
	privKey, pubKey, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		return fmt.Errorf("failed to generate VAPID keys: %w", err)
	}

	m.vapidPrivate = privKey
	m.vapidPublic = pubKey

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("failed to create config dir: %w", err)
	}

	keys := vapidKeys{
		PrivateKey: m.vapidPrivate,
		PublicKey:  m.vapidPublic,
	}
	data, _ = json.MarshalIndent(keys, "", "  ")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("failed to save VAPID keys: %w", err)
	}

	m.logger.Info("generated new VAPID keys")
	return nil
}

func (m *Manager) loadSubscriptions() {
	path := filepath.Join(configdir.Path(), subscriptionsFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			m.logger.Warn("failed to read push subscriptions", "err", err)
		}
		return
	}
	var subs []*webpush.Subscription
	if err := json.Unmarshal(data, &subs); err != nil {
		m.logger.Warn("corrupted push subscriptions file, ignoring", "path", path, "err", err)
		return
	}
	// drop entries missing required fields
	cleaned := subs[:0]
	for _, s := range subs {
		if s == nil || s.Endpoint == "" || s.Keys.Auth == "" || s.Keys.P256dh == "" {
			continue
		}
		cleaned = append(cleaned, s)
	}
	m.mu.Lock()
	m.subscriptions = cleaned
	m.mu.Unlock()
	if len(cleaned) > 0 {
		m.logger.Info("loaded push subscriptions", "count", len(cleaned))
	}
}

// persistSubscriptions writes the current subscription list to disk. Best
// effort: failures are logged but never returned, since a missed save just
// means a subscription has to re-register on next page load.
//
// persistMu serializes the snapshot+write so concurrent callers cannot
// interleave on the shared .tmp filename or commit a stale snapshot after a
// newer one.
func (m *Manager) persistSubscriptions() {
	m.persistMu.Lock()
	defer m.persistMu.Unlock()

	dir := configdir.Path()
	path := filepath.Join(dir, subscriptionsFile)

	m.mu.Lock()
	snapshot := make([]*webpush.Subscription, len(m.subscriptions))
	copy(snapshot, m.subscriptions)
	m.mu.Unlock()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		m.logger.Warn("failed to create config dir for subscriptions", "err", err)
		return
	}
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		m.logger.Warn("failed to marshal subscriptions", "err", err)
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		m.logger.Warn("failed to write push subscriptions", "err", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		m.logger.Warn("failed to rename push subscriptions file", "err", err)
		_ = os.Remove(tmp)
	}
}
