package app

import (
	"errors"
	"strings"
	"testing"
)

// stubKeychain replaces the Keychain lookup for the duration of one test.
func stubKeychain(t *testing.T, password string, err error) {
	t.Helper()
	orig := keychainPasswordGet
	keychainPasswordGet = func(_, _ string) (string, error) { return password, err }
	t.Cleanup(func() { keychainPasswordGet = orig })
}

func forwardCfg(fp *ForwarderProxyConfig, mode string) Config {
	return Config{
		CurrentContext: "ctx",
		Contexts: map[string]SwitchContext{
			"ctx": {ProxyMode: mode, ForwarderProxy: fp},
		},
	}
}

func basicCred(env []string) (string, bool) {
	for _, e := range env {
		if after, ok := strings.CutPrefix(e, "BASIC_CREDENTIALS="); ok {
			return after, true
		}
	}
	return "", false
}

func envValue(env []string, name string) (string, bool) {
	for _, e := range env {
		if after, ok := strings.CutPrefix(e, name+"="); ok {
			return after, true
		}
	}
	return "", false
}

// TestProxyEnvSuppliesBasicCredentials is the fallback the whole change is
// for: without it, a context whose Kerberos ticket is missing has no
// authentication method left and every request through the proxy fails.
func TestProxyEnvSuppliesBasicCredentials(t *testing.T) {
	stubKeychain(t, "s3cret", nil)
	cfg := forwardCfg(&ForwarderProxyConfig{
		ProxyServer:             "proxy.example.com",
		Port:                    8080,
		Username:                "user",
		PasswordKeychainService: "Corp AD User",
	}, "forward")

	env, err := proxyEnv(cfg)
	if err != nil {
		t.Fatalf("proxyEnv() error = %v", err)
	}
	got, ok := basicCred(env)
	if !ok {
		t.Fatal("BASIC_CREDENTIALS not set")
	}
	if want := "user:s3cret"; got != want {
		t.Errorf("BASIC_CREDENTIALS = %q, want %q", got, want)
	}
}

func TestProxyEnvUsesKerberosKeepAliveCache(t *testing.T) {
	writeKeepAliveConfig(t, "profiles:\n  - name: corp\n    ccache_path: /tmp/krb5cc-corp\n")
	t.Setenv("KRB5CCNAME", "API:interactive-cache")
	stubKeychain(t, "s3cret", nil)

	env, err := proxyEnv(forwardCfg(&ForwarderProxyConfig{
		ProxyServer: "proxy.example.com", Port: 8080, Username: "u", PasswordKeychainService: "s",
	}, ProxyModeForward))
	if err != nil {
		t.Fatalf("proxyEnv() error = %v", err)
	}
	if got, ok := envValue(env, "KRB5CCNAME"); !ok || got != "FILE:/tmp/krb5cc-corp" {
		t.Errorf("KRB5CCNAME = %q, present=%v; want FILE:/tmp/krb5cc-corp", got, ok)
	}
}

func TestProxyEnvOmitsCredentialsWhenNotForwarding(t *testing.T) {
	// Would panic the stub if consulted; not being consulted is the point.
	stubKeychain(t, "s3cret", nil)
	tests := []struct {
		name string
		mode string
		fp   *ForwarderProxyConfig
	}{
		{
			// Nothing to authenticate to: alpaca talks to the filter proxy
			// or straight out, and a password in the environment would be
			// exposure without purpose.
			name: "direct mode",
			mode: "direct",
			fp:   &ForwarderProxyConfig{ProxyServer: "p", Port: 8080, Username: "u", PasswordKeychainService: "s"},
		},
		{
			name: "forward mode without a forwarder_proxy",
			mode: "forward",
			fp:   nil,
		},
		{
			name: "forward mode without a username",
			mode: "forward",
			fp:   &ForwarderProxyConfig{ProxyServer: "p", Port: 8080, PasswordKeychainService: "s"},
		},
		{
			name: "forward mode without a keychain service",
			mode: "forward",
			fp:   &ForwarderProxyConfig{ProxyServer: "p", Port: 8080, Username: "u"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, err := proxyEnv(forwardCfg(tt.fp, tt.mode))
			if err != nil {
				t.Fatalf("proxyEnv() error = %v", err)
			}
			if cred, ok := basicCred(env); ok {
				t.Errorf("BASIC_CREDENTIALS unexpectedly set to %q", cred)
			}
		})
	}
}

// TestProxyEnvFailsLoudlyOnKeychainError: a Keychain that cannot be read is
// the operator's problem to fix, and starting a proxy that will reject every
// request hides it.
func TestProxyEnvFailsLoudlyOnKeychainError(t *testing.T) {
	stubKeychain(t, "", errors.New("keychain locked"))
	cfg := forwardCfg(&ForwarderProxyConfig{
		ProxyServer: "p", Port: 8080, Username: "u", PasswordKeychainService: "s",
	}, "forward")
	if _, err := proxyEnv(cfg); err == nil {
		t.Fatal("proxyEnv() error = nil, want the Keychain failure surfaced")
	}
}
