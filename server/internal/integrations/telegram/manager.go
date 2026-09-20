package telegram

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Runtime is the slice of handler-bound services that only exist while the
// Telegram integration is enabled. The router's OnRuntimeChange callback
// copies these onto the Handler, so every existing handler check
// (`h.TelegramInstall == nil` → 503) keeps working across enable/disable
// transitions.
type Runtime struct {
	Install *InstallService
}

// managerQueries is the slice of generated queries Manager needs, interface-
// shaped so tests inject a fake (same adapter pattern as InstallService).
type managerQueries interface {
	GetTelegramMasterConfig(ctx context.Context) (db.GetTelegramMasterConfigRow, error)
	UpsertTelegramMasterConfig(ctx context.Context, arg db.UpsertTelegramMasterConfigParams) (db.TelegramMasterConfig, error)
	DeleteTelegramMasterConfig(ctx context.Context) error
	// RevokeAllActive uses the shared channel_* queries (no box required) so
	// disabling the integration tears every polling loop down cleanly.
	ListAllActiveChannelInstallations(ctx context.Context) ([]db.ChannelInstallation, error)
	SetChannelInstallationStatus(ctx context.Context, arg db.SetChannelInstallationStatusParams) error
}

type dbManagerQueries struct{ *db.Queries }

func (q dbManagerQueries) GetTelegramMasterConfig(ctx context.Context) (db.GetTelegramMasterConfigRow, error) {
	return q.Queries.GetTelegramMasterConfig(ctx)
}

func (q dbManagerQueries) UpsertTelegramMasterConfig(ctx context.Context, arg db.UpsertTelegramMasterConfigParams) (db.TelegramMasterConfig, error) {
	return q.Queries.UpsertTelegramMasterConfig(ctx, arg)
}

func (q dbManagerQueries) DeleteTelegramMasterConfig(ctx context.Context) error {
	return q.Queries.DeleteTelegramMasterConfig(ctx)
}

// Manager controls the run-time state of the Telegram integration. It is the
// single place the deployment master key (secretbox at-rest key) is held,
// swapped, and persisted.
//
// Every Telegram transport (outbound, replier, typing, per-installation
// channel Factory) is wired ONCE at boot over the dynamic Decrypt closure, so
// enabling/disabling at runtime never requires re-registering bus subscribers
// or channel factories. Enable builds a fresh Box + InstallService; Disable
// clears them and revokes every active installation so the Supervisor's
// polling loops wind down (the Engine's existing "revoked stops the loop"
// primitive — no new teardown path needed).
//
// The master key may come from MULTICA_TELEGRAM_SECRET_KEY (boot, env-only,
// never persisted) or from the single-row telegram_master_config table
// (admin UI; persisted so it survives restarts). Whichever is valid at boot
// enables the integration.
type Manager struct {
	q    managerQueries
	base *db.Queries
	tx   engine.TxStarter
	log  *slog.Logger
	sink func(*Runtime)

	mu      sync.Mutex
	enabled bool
	box     *secretbox.Box
	install *InstallService
}

// ManagerConfig carries the dependencies NewManager needs plus the callback
// that publishes the current Runtime onto the Handler.
type ManagerConfig struct {
	Queries *db.Queries
	Tx      engine.TxStarter
	Logger  *slog.Logger
	// OnRuntimeChange is invoked with the current Runtime whenever the
	// integration's enable state transitions. A nil Runtime means disabled.
	OnRuntimeChange func(*Runtime)
}

// NewManager builds the Telegram integration manager. Callers should keep the
// returned *Manager for the process lifetime; EnableKey / StoreKeyAndEnable /
// Disable are safe to call concurrently (serialized on an internal mutex).
func NewManager(cfg ManagerConfig) *Manager {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		q:    dbManagerQueries{cfg.Queries},
		base: cfg.Queries,
		tx:   cfg.Tx,
		log:  logger,
		sink: cfg.OnRuntimeChange,
	}
}

// Decrypt is the Decrypter every Telegram transport closes over. It reads the
// current Box atomically, so a runtime toggle takes effect for already-wired
// outbound/replier/typing/channel code without any re-registration.
func (m *Manager) Decrypt(ciphertext []byte) ([]byte, error) {
	m.mu.Lock()
	box := m.box
	m.mu.Unlock()
	if box == nil {
		return nil, errors.New("telegram: integration disabled")
	}
	return box.Open(ciphertext)
}

// Configured reports whether the integration is currently enabled (a valid
// master key is loaded). Drives the management-API `configured` flag.
func (m *Manager) Configured() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.enabled
}

// EnabledInstall returns the current InstallService, or nil while disabled.
func (m *Manager) EnabledInstall() *InstallService {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.install
}

// ParseMasterKey decodes and validates a base64-encoded 32-byte master key
// pasted in the admin UI. The value is never echoed back by any endpoint.
func ParseMasterKey(b64 string) ([]byte, error) {
	raw := strings.TrimSpace(b64)
	if raw == "" {
		return nil, errors.New("master key is required")
	}
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("master key must be base64 (openssl rand -base64 32): %w", err)
	}
	if len(key) != secretbox.KeySize {
		return nil, fmt.Errorf("master key must decode to %d bytes, got %d", secretbox.KeySize, len(key))
	}
	return key, nil
}

// EnableKey enables the runtime with an already-validated master key WITHOUT
// persisting it. Boot uses it for the env-var key and for a previously stored
// DB row; the admin UI uses StoreKeyAndEnable so the key survives restarts.
func (m *Manager) EnableKey(ctx context.Context, key []byte) error {
	box, err := secretbox.New(key)
	if err != nil {
		return fmt.Errorf("telegram: build encryption box: %w", err)
	}
	install, err := NewInstallService(m.base, m.tx, box, m.log)
	if err != nil {
		return fmt.Errorf("telegram: build install service: %w", err)
	}
	runtime := &Runtime{Install: install}
	m.mu.Lock()
	m.box = box
	m.install = install
	m.enabled = true
	m.mu.Unlock()
	m.publish(runtime)
	m.log.Info("telegram integration enabled")
	return nil
}

// StoreKeyAndEnable persists the master key to the single-row
// telegram_master_config table, then hot-enables the runtime. The admin UI
// path. Persist first so a crash between the write and the enable still boots
// the integration on the next start; a failed enable rolls the row back.
func (m *Manager) StoreKeyAndEnable(ctx context.Context, key []byte, updatedBy pgtype.UUID) error {
	if len(key) != secretbox.KeySize {
		return secretbox.ErrInvalidKey
	}
	if _, err := m.q.UpsertTelegramMasterConfig(ctx, db.UpsertTelegramMasterConfigParams{
		SecretKeyBase64: base64.StdEncoding.EncodeToString(key),
		UpdatedBy:       updatedBy,
	}); err != nil {
		return fmt.Errorf("telegram: persist master key: %w", err)
	}
	if err := m.EnableKey(ctx, key); err != nil {
		// Left the row behind on a partially-applied enable: roll it back so
		// a restart does not silently enable with a key the UI reported as
		// failed.
		_ = m.q.DeleteTelegramMasterConfig(ctx)
		return err
	}
	return nil
}

// Disable hot-disables the integration: clears the Box + InstallService,
// revokes every active Telegram installation (stopping their polling loops
// via the Supervisor's normal revoked status), and deletes the stored master
// key row. Chat history and installations are preserved (rows stay, status
// flips); reconnecting a bot requires re-pasting its token.
func (m *Manager) Disable(ctx context.Context) error {
	m.mu.Lock()
	wasEnabled := m.enabled
	m.enabled = false
	m.box = nil
	m.install = nil
	m.mu.Unlock()
	m.publish(nil)
	if wasEnabled {
		m.revokeAllActive(ctx)
	}
	if err := m.q.DeleteTelegramMasterConfig(ctx); err != nil {
		m.log.Warn("telegram: failed to clear stored master key", "error", err)
	}
	m.log.Info("telegram integration disabled")
	return nil
}

// LoadStoredKey decodes the persisted master key, if present and valid.
// Returns (nil, false) when no usable key is stored.
func (m *Manager) LoadStoredKey(ctx context.Context) ([]byte, bool) {
	row, err := m.q.GetTelegramMasterConfig(ctx)
	if err != nil {
		return nil, false
	}
	key, err := ParseMasterKey(row.SecretKeyBase64)
	if err != nil {
		m.log.Error("telegram: stored master key is invalid; telegram integration disabled", "error", err)
		return nil, false
	}
	return key, true
}

// publish runs the handler-bound runtime transition outside the internal
// mutex so a slow consumer never blocks Enable/Disable.
func (m *Manager) publish(runtime *Runtime) {
	if m.sink != nil {
		m.sink(runtime)
	}
}

// revokeAllActive flips every active Telegram installation to 'revoked' so
// the engine Supervisor stops supervising each polling loop. Best-effort:
// failures are logged, never fatal to the disable itself.
func (m *Manager) revokeAllActive(ctx context.Context) {
	rows, err := m.q.ListAllActiveChannelInstallations(ctx)
	if err != nil {
		m.log.Error("telegram: failed to list active installations for disable", "error", err)
		return
	}
	for _, row := range rows {
		if row.ChannelType != string(TypeTelegram) {
			continue
		}
		if err := m.q.SetChannelInstallationStatus(ctx, db.SetChannelInstallationStatusParams{
			ID:     row.ID,
			Status: "revoked",
		}); err != nil {
			m.log.Error("telegram: failed to revoke installation on disable", "error", err, "installation_id", row.ID)
		}
	}
}