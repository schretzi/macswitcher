package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeContextConfig lays out the on-disk shape loadConfig expects: a global
// file plus one file per context.
func writeContextConfig(t *testing.T, contexts map[string]string) string {
	t.Helper()

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "contexts"), 0o750); err != nil {
		t.Fatal(err)
	}
	global := "current_context: home\nlocal_proxy:\n  host: 127.0.0.2\n  port: 3128\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(global), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, body := range contexts {
		if err := os.WriteFile(filepath.Join(dir, "contexts", name+".yaml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "config.yaml")
}

// The whole reason ProtectionEnabled is a *bool. A plain bool cannot tell
// "the context says false" from "the context never mentions it", and its zero
// value would turn filtering off on every context written before the setting
// existed - silently, and everywhere, including at home.
func TestLoadConfigProtectionEnabledIsThreeState(t *testing.T) {
	t.Parallel()

	cfgPath := writeContextConfig(t, map[string]string{
		"home":    "proxy_mode: direct\n",
		"office":  "proxy_mode: forward\nprotection_enabled: false\n",
		"private": "proxy_mode: direct\nprotection_enabled: true\n",
	})

	cfg, err := loadConfig(cfgPath)
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}

	if got := cfg.Contexts["home"].ProtectionEnabled; got != nil {
		t.Fatalf("a context that omits protection_enabled must leave AdGuard alone, got %v", *got)
	}
	office := cfg.Contexts["office"].ProtectionEnabled
	if office == nil || *office {
		t.Fatalf("office: want explicit false, got %v", office)
	}
	private := cfg.Contexts["private"].ProtectionEnabled
	if private == nil || !*private {
		t.Fatalf("private: want explicit true, got %v", private)
	}
}

// adguardTestServer stands in for AdGuard Home, recording what it was asked to
// do and asserting the call is authenticated.
type adguardTestServer struct {
	server    *httptest.Server
	calls     []bool
	user      string
	pass      string
	failFirst int
}

func newAdGuardTestServer(t *testing.T, user, pass string, failFirst int) *adguardTestServer {
	t.Helper()

	s := &adguardTestServer{user: user, pass: pass, failFirst: failFirst}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, ok := r.BasicAuth()
		if !ok || gotUser != s.user || gotPass != s.pass {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/control/protection":
			if len(s.calls) < s.failFirst {
				s.calls = append(s.calls, false)
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			body, _ := io.ReadAll(r.Body)
			var payload struct {
				Enabled bool `json:"enabled"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			s.calls = append(s.calls, payload.Enabled)
			w.WriteHeader(http.StatusOK)
		case "/control/status":
			_ = json.NewEncoder(w).Encode(map[string]any{"protection_enabled": true})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(s.server.Close)
	return s
}

func (s *adguardTestServer) config() Config {
	return Config{AdGuard: AdGuardConfig{
		Address:            strings.TrimPrefix(s.server.URL, "http://"),
		APIUser:            s.user,
		APIKeychainService: "test",
	}}
}

// stubAdGuardPassword replaces the System-keychain lookup for the duration of
// a test, and shortens the retry budget so a failure path does not spend the
// real twenty seconds.
func stubAdGuardPassword(t *testing.T, pass string) {
	t.Helper()

	originalPassword := adguardAPIPassword
	originalRetry := adguardAPIRetryFor
	originalInterval := adguardAPIRetryInterval
	adguardAPIPassword = func(Config) (string, error) { return pass, nil }
	adguardAPIRetryFor = 200 * time.Millisecond
	adguardAPIRetryInterval = 10 * time.Millisecond
	t.Cleanup(func() {
		adguardAPIPassword = originalPassword
		adguardAPIRetryFor = originalRetry
		adguardAPIRetryInterval = originalInterval
	})
}

func TestSetAdGuardProtectionSendsRequestedState(t *testing.T) {
	stubAdGuardPassword(t, "secret")
	srv := newAdGuardTestServer(t, "zonesync", "secret", 0)

	if err := setAdGuardProtection(srv.config(), false); err != nil {
		t.Fatalf("setAdGuardProtection() error = %v", err)
	}
	if len(srv.calls) != 1 || srv.calls[0] {
		t.Fatalf("want one call disabling protection, got %v", srv.calls)
	}
}

// AdGuard Home is restarted immediately before this runs, and launchctl
// returns before its HTTP listener is up - so the first calls being refused is
// the normal case, not a failure.
func TestSetAdGuardProtectionRetriesWhileAdGuardIsRestarting(t *testing.T) {
	stubAdGuardPassword(t, "secret")
	srv := newAdGuardTestServer(t, "zonesync", "secret", 2)

	if err := setAdGuardProtection(srv.config(), true); err != nil {
		t.Fatalf("setAdGuardProtection() error = %v", err)
	}
	if len(srv.calls) != 3 || !srv.calls[2] {
		t.Fatalf("want two refusals then a success, got %v", srv.calls)
	}
}

func TestSetAdGuardProtectionFailsOnBadCredential(t *testing.T) {
	stubAdGuardPassword(t, "wrong")
	srv := newAdGuardTestServer(t, "zonesync", "secret", 0)

	err := setAdGuardProtection(srv.config(), false)
	if err == nil {
		t.Fatal("setAdGuardProtection() accepted a rejected credential")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("error should report the rejection, got %v", err)
	}
}

// A context that never mentions protection_enabled must not produce a call at
// all - not one that happens to send the current state.
func TestSyncAdGuardProtectionSkipsWhenUnset(t *testing.T) {
	stubAdGuardPassword(t, "secret")
	srv := newAdGuardTestServer(t, "zonesync", "secret", 0)

	syncAdGuardProtection(srv.config(), SwitchContext{})
	if len(srv.calls) != 0 {
		t.Fatalf("want no API call for an unset context, got %v", srv.calls)
	}
}

// The DNS-check hint is what turns "DNS is broken" into a diagnosis. It is
// worth showing only where filtering is still a candidate cause.
func TestProtectionHintOnlyWhereFilteringCouldBeTheCause(t *testing.T) {
	t.Parallel()

	disabled := false
	if got := protectionHint(SwitchContext{ProtectionEnabled: &disabled}); got != "" {
		t.Fatalf("a context that already disables filtering needs no hint, got %q", got)
	}
	if got := protectionHint(SwitchContext{}); !strings.Contains(got, "protection_enabled: false") {
		t.Fatalf("hint should name the setting, got %q", got)
	}
	enabled := true
	if got := protectionHint(SwitchContext{ProtectionEnabled: &enabled}); !strings.Contains(got, "protection_enabled: false") {
		t.Fatalf("a context asking for filtering should still get the hint, got %q", got)
	}
}

func TestAdGuardAPIURLNormalisesAddress(t *testing.T) {
	t.Parallel()

	for _, addr := range []string{"127.0.0.3:3053", "http://127.0.0.3:3053", "http://127.0.0.3:3053/"} {
		cfg := Config{AdGuard: AdGuardConfig{Address: addr}}
		if got := adguardAPIURL(cfg, "/control/protection"); got != "http://127.0.0.3:3053/control/protection" {
			t.Fatalf("adguardAPIURL(%q) = %q", addr, got)
		}
	}
	if got := adguardAPIURL(Config{}, "/control/status"); got != "http://"+defaultAdGuardAddress+"/control/status" {
		t.Fatalf("empty address should fall back to the default, got %q", got)
	}
}
