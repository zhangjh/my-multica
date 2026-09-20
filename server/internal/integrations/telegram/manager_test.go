package telegram

import (
	"context"
	"encoding/base64"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/util/secretbox"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// fakeTelegramManagerQueries implements managerQueries with an in-memory
// single-row store, mirroring fakeTelegramInstallQueries (same adapter
// pattern). The tx starter is intentionally a stub: the manager never opens a
// transaction on these paths.
type fakeTelegramManagerQueries struct {
	mu      sync.Mutex
	stored  *db.TelegramMasterConfig
	active  []db.ChannelInstallation
	revoked []pgtype.UUID
}

func (f *fakeTelegramManagerQueries) GetTelegramMasterConfig(context.Context) (db.GetTelegramMasterConfigRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stored == nil {
		return db.GetTelegramMasterConfigRow{}, pgx.ErrNoRows
	}
	return db.GetTelegramMasterConfigRow{
		SecretKeyBase64: f.stored.SecretKeyBase64,
		UpdatedBy:       f.stored.UpdatedBy,
		UpdatedAt:       f.stored.UpdatedAt,
	}, nil
}

func (f *fakeTelegramManagerQueries) UpsertTelegramMasterConfig(_ context.Context, arg db.UpsertTelegramMasterConfigParams) (db.TelegramMasterConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row := db.TelegramMasterConfig{
		ID:              true,
		SecretKeyBase64: arg.SecretKeyBase64,
		UpdatedBy:       arg.UpdatedBy,
	}
	f.stored = &row
	return row, nil
}

func (f *fakeTelegramManagerQueries) DeleteTelegramMasterConfig(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stored = nil
	return nil
}

func (f *fakeTelegramManagerQueries) ListAllActiveChannelInstallations(context.Context) ([]db.ChannelInstallation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.active, nil
}

func (f *fakeTelegramManagerQueries) SetChannelInstallationStatus(_ context.Context, arg db.SetChannelInstallationStatusParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked = append(f.revoked, arg.ID)
	return nil
}

func newManagerTestKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, secretbox.KeySize)
	for i := range key {
		key[i] = byte(i + 7)
	}
	return key
}

func managerTestUUID(v byte) pgtype.UUID {
	var b [16]byte
	b[0] = v
	return pgtype.UUID{Bytes: b, Valid: true}
}

// Sink transitions drive the handler republish; keep them observable.
func newManagerTestManager(t *testing.T, fq *fakeTelegramManagerQueries) (*Manager, *[]*Runtime) {
	t.Helper()
	var transitions []*Runtime
	m := NewManager(ManagerConfig{
		Queries: &db.Queries{},
		Tx:      fakeTelegramTxStarter{tx: &fakeTelegramTx{}},
		OnRuntimeChange: func(r *Runtime) {
			transitions = append(transitions, r)
		},
	})
	// Swap the real adapter for the fake so the test can drive persistence and
	// revocation without a database.
	m.q = fq
	return m, &transitions
}

func TestManagerParseMasterKey(t *testing.T) {
	valid := base64.StdEncoding.EncodeToString(newManagerTestKey(t))
	if _, err := ParseMasterKey(valid); err != nil {
		t.Fatalf("valid key rejected: %v", err)
	}
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"whitespace only", "   \n\t"},
		{"not base64", "!!!not-base64!!!"},
		{"too short", base64.StdEncoding.EncodeToString(make([]byte, 16))},
		{"too long", base64.StdEncoding.EncodeToString(make([]byte, 33))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseMasterKey(tc.in); err == nil {
				t.Fatalf("expected error for input %q", tc.in)
			}
		})
	}
}

func TestManagerEnableDisableTogglesCryptoToggle(t *testing.T) {
	fq := &fakeTelegramManagerQueries{}
	m, transitions := newManagerTestManager(t, fq)

	if m.Configured() {
		t.Fatal("manager starts enabled; want disabled")
	}
	if err := m.EnableKey(context.Background(), newManagerTestKey(t)); err != nil {
		t.Fatalf("EnableKey: %v", err)
	}
	if !m.Configured() {
		t.Fatal("manager should be enabled after EnableKey")
	}
	if m.EnabledInstall() == nil {
		t.Fatal("EnabledInstall should be non-nil while enabled")
	}
	if got := len(*transitions); got != 1 || (*transitions)[0] == nil || (*transitions)[0].Install == nil {
		t.Fatalf("expected one enabled runtime transition, got %d", got)
	}

	// The dynamic decrypt closure must round-trip real sealed bytes without a
	// re-registration — that is the whole point of the Manager.
	plain := []byte("1282456790:AAF-bot-token")
	builder, err := secretbox.New(newManagerTestKey(t))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := builder.Seal(plain)
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.Decrypt(sealed)
	if err != nil {
		t.Fatalf("Decrypt while enabled: %v", err)
	}
	if string(got) != string(plain) {
		t.Fatalf("Decrypt round-trip mismatch: %q != %q", got, plain)
	}

	if err := m.Disable(context.Background()); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if m.Configured() {
		t.Fatal("manager should be disabled after Disable")
	}
	if m.EnabledInstall() != nil {
		t.Fatal("EnabledInstall should be nil while disabled")
	}
	if _, err := m.Decrypt(sealed); err == nil {
		t.Fatal("Decrypt while disabled should error")
	}
	if last := (*transitions)[len(*transitions)-1]; last != nil {
		t.Fatal("expected a nil runtime transition after disable")
	}
}

func TestManagerDisableRevokesOnlyTelegramInstalls(t *testing.T) {
	fq := &fakeTelegramManagerQueries{
		active: []db.ChannelInstallation{
			{ID: managerTestUUID(1), ChannelType: string(TypeTelegram)},
			{ID: managerTestUUID(2), ChannelType: string(TypeTelegram)},
			{ID: managerTestUUID(3), ChannelType: "slack"},
		},
	}
	m, _ := newManagerTestManager(t, fq)
	if err := m.EnableKey(context.Background(), newManagerTestKey(t)); err != nil {
		t.Fatalf("EnableKey: %v", err)
	}
	if err := m.Disable(context.Background()); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if len(fq.revoked) != 2 {
		t.Fatalf("expected exactly the 2 telegram installs revoked, got %d (%v)", len(fq.revoked), fq.revoked)
	}
}

func TestManagerStoreKeyAndEnablePersistsThenLoads(t *testing.T) {
	fq := &fakeTelegramManagerQueries{}
	m, _ := newManagerTestManager(t, fq)
	key := newManagerTestKey(t)
	by := pgtype.UUID{Bytes: [16]byte{1}, Valid: true}

	if err := m.StoreKeyAndEnable(context.Background(), key, by); err != nil {
		t.Fatalf("StoreKeyAndEnable: %v", err)
	}
	fq.mu.Lock()
	stored := fq.stored
	fq.mu.Unlock()
	if stored == nil || stored.SecretKeyBase64 != base64.StdEncoding.EncodeToString(key) {
		t.Fatalf("expected the key to be persisted, got %+v", stored)
	}

	// A fresh manager over the same store should boot enabled from the row.
	m2, _ := newManagerTestManager(t, fq)
	loaded, ok := m2.LoadStoredKey(context.Background())
	if !ok {
		t.Fatal("LoadStoredKey should find the stored row")
	}
	if err := m2.EnableKey(context.Background(), loaded); err != nil {
		t.Fatalf("EnableKey from stored key: %v", err)
	}
	if !m2.Configured() {
		t.Fatal("fresh manager should be enabled after loading stored key")
	}

	// StoreKeyAndEnable with an invalid length must fail without persisting.
	if err := m.StoreKeyAndEnable(context.Background(), make([]byte, 8), by); err == nil {
		t.Fatal("expected error for short key")
	}
}

func TestManagerLoadStoredKeyIgnoresCorruptRow(t *testing.T) {
	fq := &fakeTelegramManagerQueries{
		stored: &db.TelegramMasterConfig{ID: true, SecretKeyBase64: "definitely-not-a-key"},
	}
	m, _ := newManagerTestManager(t, fq)
	if _, ok := m.LoadStoredKey(context.Background()); ok {
		t.Fatal("corrupt stored key should not load")
	}
}

func TestManagerDecryptErrorText(t *testing.T) {
	m, _ := newManagerTestManager(t, &fakeTelegramManagerQueries{})
	if _, err := m.Decrypt(nil); err == nil {
		t.Fatal("expected error when disabled")
	} else if errors.Is(err, secretbox.ErrCiphertextTooShort) {
		t.Fatalf("disabled error should not leak a crypto error: %v", err)
	}
}