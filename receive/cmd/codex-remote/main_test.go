package main

import "testing"

func TestPublicListenModes(t *testing.T) {
	tests := []struct {
		name            string
		listen          string
		directTLS       bool
		allowPublicHTTP bool
		trustedProxy    bool
		allowed         bool
	}{
		{name: "loopback HTTP", listen: "127.0.0.1:18787", allowed: true},
		{name: "public direct TLS", listen: "0.0.0.0:18787", directTLS: true, allowed: true},
		{name: "public explicit HTTP", listen: "0.0.0.0:18787", allowPublicHTTP: true, allowed: true},
		{name: "public TLS proxy", listen: "0.0.0.0:18787", trustedProxy: true, allowed: true},
		{name: "public implicit HTTP", listen: "0.0.0.0:18787", allowed: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			allowed := listenerSecurityConfigured(test.listen, test.directTLS, test.allowPublicHTTP, test.trustedProxy)
			if allowed != test.allowed {
				t.Fatalf("listen mode allowed = %v, want %v", allowed, test.allowed)
			}
		})
	}
}
