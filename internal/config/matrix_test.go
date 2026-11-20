package config

import "testing"

func TestMatrixOnNeedsTransportSecurity(t *testing.T) {
	for addr, want := range map[string]bool{
		"":                           false,
		"https://matrix.example.org": true,
		"http://matrix.example.org":  false, // the token would cross the internet in the clear
		"http://localhost:8008":      true,
		"http://127.0.0.1:8008":      true,
		"http://[::1]:8008":          true,
		"http://192.168.1.20:8008":   true,
		"http://synapse:8008":        true, // a Docker service on the same network
		"http://8.8.8.8":             false,
		"ftp://matrix.example.org":   false,
		"matrix.example.org":         false,
	} {
		if got := (&Config{MatrixHomeserver: addr}).MatrixOn(); got != want {
			t.Errorf("%q: MatrixOn %v, want %v", addr, got, want)
		}
	}
}
