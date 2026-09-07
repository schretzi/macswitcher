package app

import "testing"

// contextStartsVPN decides whether AdGuard Home's upstreams may be rewritten
// before preflight runs (see switchContext). Only a context that starts the
// "vpn" application depends on the OLD network's resolvers to bring up its
// own gateway, so only that case must keep the old ordering.
func TestContextStartsVPN(t *testing.T) {
	cases := []struct {
		name  string
		start []string
		want  bool
	}{
		{name: "no apps", start: nil, want: false},
		{name: "office-like", start: []string{"kerberos_keep_alive"}, want: false},
		{name: "vpn present", start: []string{"vpn", "kerberos_keep_alive"}, want: true},
		{name: "case and whitespace insensitive", start: []string{" VPN "}, want: true},
		{name: "unrelated app named vpnish", start: []string{"vpnish"}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := SwitchContext{Apps: LifecycleConfig{Start: tc.start}}
			if got := contextStartsVPN(ctx); got != tc.want {
				t.Fatalf("contextStartsVPN(%v) = %v, want %v", tc.start, got, tc.want)
			}
		})
	}
}
