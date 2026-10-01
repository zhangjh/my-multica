package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Regression for the boot that reported itself healthy with no agent runtime.
//
// start.sh starts the daemon right after the backend it depends on, so at boot
// the daemon's very first API call routinely lands while the server — or, when
// the configured URL is a public one, its edge — is still coming up. That first
// call answered 502, preflightAuth returned the error, Run returned it, and the
// process exited 1 about five seconds in with no retry anywhere behind it. The
// two-minute self-heal timer then retried the same doomed call six more times
// over seventeen minutes before the edge came up.
//
// Every other part of the daemon already rides out exactly that outage (the
// task wakeup socket reconnects indefinitely, the workspace sync backs off,
// the terminal callbacks retry), so startup was the one place that did not.
func TestPreflightAuth_RetriesTransientServerOutage(t *testing.T) {
	defer noSleepRetry(t)()

	var syncs atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tokens/current/renew":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"expires_at": "2099-01-02T03:04:05Z",
				"renewed":    false,
			})
		case "/api/daemon/workspaces":
			// A Cloudflare-fronted origin that has not finished booting.
			if syncs.Add(1) <= 3 {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte(`error code: 502`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	var buf bytes.Buffer
	d := &Daemon{client: NewClient(srv.URL), logger: captureLogger(&buf)}
	d.client.SetToken("mul_boot_race")

	if err := d.preflightAuth(context.Background()); err != nil {
		t.Fatalf("a server that is still coming up must not kill startup: %v", err)
	}
	if got := syncs.Load(); got != 4 {
		t.Fatalf("workspace sync attempts = %d, want 4 (three 502s then success)", got)
	}
	// The operator has to be able to see why startup took a while, so the wait
	// is announced rather than silent.
	if !strings.Contains(buf.String(), "the server may still be starting") {
		t.Fatalf("expected the retry to be logged, got: %s", buf.String())
	}
}

// The other side of the retry: a rejected PAT is not an outage, and stalling on
// it would bury the one hint that fixes it.
func TestPreflightAuth_RejectedTokenStillFailsFast(t *testing.T) {
	defer noSleepRetry(t)()

	var syncs atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/daemon/workspaces" {
			syncs.Add(1)
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid token"}`))
	}))
	t.Cleanup(srv.Close)

	var buf bytes.Buffer
	d := &Daemon{client: NewClient(srv.URL), logger: captureLogger(&buf)}
	d.client.SetToken("mul_already_revoked")

	if err := d.preflightAuth(context.Background()); err == nil {
		t.Fatal("expected a 401 to fail startup")
	}
	if got := syncs.Load(); got != 1 {
		t.Fatalf("workspace sync attempts = %d, want 1 — a permanent failure must not be retried", got)
	}
	if !strings.Contains(buf.String(), "multica login") {
		t.Fatalf("expected the actionable re-login hint, got: %s", buf.String())
	}
}

// The retry must stay bounded. An outage that outlives the window still fails
// startup so something above it (the self-heal timer) can take over, rather
// than the daemon waiting on the server forever.
func TestPreflightAuth_GivesUpAfterTheRetryBudget(t *testing.T) {
	defer noSleepRetry(t)()

	prev := preflightSyncMaxWait
	preflightSyncMaxWait = time.Millisecond
	defer func() { preflightSyncMaxWait = prev }()

	var syncs atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/daemon/workspaces" {
			syncs.Add(1)
		}
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`error code: 502`))
	}))
	t.Cleanup(srv.Close)

	var buf bytes.Buffer
	d := &Daemon{client: NewClient(srv.URL), logger: captureLogger(&buf)}
	d.client.SetToken("mul_long_outage")

	if err := d.preflightAuth(context.Background()); err == nil {
		t.Fatal("an outage that outlives the budget must still fail startup")
	}
	if got := syncs.Load(); got != 1 {
		t.Fatalf("workspace sync attempts = %d, want 1 — the budget is already spent", got)
	}
}

// shouldRetryStartupSync encodes the precedence between the three reasons to
// stop retrying, including the one that only looks like a transient error.
func TestShouldRetryStartupSync(t *testing.T) {
	// A bare sentinel, not an HTTP error, so isTransientError calls it
	// transient. Retrying it would be pointless — and would rob
	// startupMayProceedWithoutRuntimes of the error it needs to let that
	// startup continue.
	bootstrap := fmt.Errorf("%w for any of the %d workspace(s)", errNoWorkspaceRuntimesRegistered, 1)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	cases := []struct {
		name string
		ctx  context.Context
		d    *Daemon
		err  error
		want bool
	}{
		{"success is not retried", context.Background(), &Daemon{}, nil, false},
		{
			name: "an unreachable or 5xx server is retried",
			ctx:  context.Background(), d: &Daemon{},
			err: &requestError{StatusCode: http.StatusBadGateway}, want: true,
		},
		{
			name: "a rejected token is not",
			ctx:  context.Background(), d: &Daemon{},
			err: &requestError{StatusCode: http.StatusUnauthorized}, want: false,
		},
		{
			name: "shutting down mid-preflight is not",
			ctx:  cancelled, d: &Daemon{},
			err: &requestError{StatusCode: http.StatusBadGateway}, want: false,
		},
		{
			// The DSH bootstrap sentinel with no install running: a genuinely
			// empty machine, where the answer is not coming. It looks transient
			// to isTransientError (a bare sentinel, not an HTTP error), so
			// without an explicit exclusion it would idle out the whole budget
			// before failing.
			name: "an empty machine is not retried",
			ctx:  context.Background(), d: &Daemon{}, err: bootstrap, want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.d.shouldRetryStartupSync(tc.ctx, tc.err); got != tc.want {
				t.Fatalf("shouldRetryStartupSync(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}

	t.Run("the bootstrap still earns its exception while the install runs", func(t *testing.T) {
		d := &Daemon{}
		d.dshInstallInFlight.Store(true)
		if d.shouldRetryStartupSync(context.Background(), bootstrap) {
			t.Error("an in-flight install is not something a retry can fix")
		}
		if !d.startupMayProceedWithoutRuntimes(bootstrap) {
			t.Error("the loop handed the error back, so preflightAuth must still be allowed to proceed")
		}
	})
}

func TestPreflightSyncRetryDelay(t *testing.T) {
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 2 * time.Second},
		{1, 4 * time.Second},
		{2, 8 * time.Second},
		{3, 16 * time.Second},
		// Saturates rather than doubling past the cap.
		{4, 30 * time.Second},
		{20, 30 * time.Second},
	}
	for _, tc := range cases {
		if got := preflightSyncRetryDelay(tc.attempt); got != tc.want {
			t.Errorf("preflightSyncRetryDelay(%d) = %v, want %v", tc.attempt, got, tc.want)
		}
	}
}

// The retry window has to outlast a slow start without turning startup into a
// hang: bounded, but long enough to cover a server still coming up.
func TestPreflightSyncMaxWaitIsBounded(t *testing.T) {
	if preflightSyncMaxWait <= 0 {
		t.Fatal("the retry budget must be positive")
	}
	if preflightSyncMaxWait > 30*time.Minute {
		t.Fatalf("retry budget %v is long enough that an operator reads it as a hang", preflightSyncMaxWait)
	}
	// Every attempt inside the budget has to fit, or the loop's deadline check
	// is the only thing bounding it.
	if preflightSyncRetryMaxDelay >= preflightSyncMaxWait {
		t.Fatalf("max delay %v must be shorter than the budget %v", preflightSyncRetryMaxDelay, preflightSyncMaxWait)
	}
}
