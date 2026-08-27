package app

import "testing"

func TestDscacheutilOutputHasAddress(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		out  string
		want bool
	}{
		{
			name: "ipv4 address present",
			out:  "name: google.com\nip_address: 142.251.110.113\n",
			want: true,
		},
		{
			name: "ipv6 address present",
			out:  "name: google.com\nipv6_address: 2a00:1450:4001:c15::8b\n",
			want: true,
		},
		{
			name: "no address (unresolvable host)",
			out:  "",
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := dscacheutilOutputHasAddress(tt.out); got != tt.want {
				t.Fatalf("dscacheutilOutputHasAddress(%q) = %v, want %v", tt.out, got, tt.want)
			}
		})
	}
}

func TestCheckDNSResolutionForwardModeRequiresProxyServer(t *testing.T) {
	t.Parallel()

	ctx := SwitchContext{
		ProxyMode:      ProxyModeForward,
		ForwarderProxy: &ForwarderProxyConfig{},
	}
	err := checkDNSResolution(ctx)
	if err == nil {
		t.Fatal("expected an error when forward mode has no proxy_server configured")
	}
}
