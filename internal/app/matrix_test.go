package app

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/theshield2594/thicket/internal/channel"
	"github.com/theshield2594/thicket/internal/config"
	"github.com/theshield2594/thicket/internal/secrets"
)

// Matrix is off unless both the homeserver and an access token are set;
// with them, it starts alongside the other chat apps and its token is
// stored encrypted.
func TestMatrixStartsOnlyWhenConfigured(t *testing.T) {
	const tok = "syt_dGhpY2tldA_abcdefghijklmnopqrst_0AbCdE"
	for _, tc := range []struct {
		name, homeserver string
		want             bool
	}{
		{"unset", "", false},
		{"set", "https://matrix.example.org", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("THICKET_DATA_DIR", dir)
			t.Setenv("DISCORD_TOKEN", "") // whatever the developer's shell has
			t.Setenv("THICKET_MATRIX_HOMESERVER", tc.homeserver)
			t.Setenv("THICKET_MATRIX_TOKEN", tok)
			t.Setenv("TELEGRAM_TOKEN", "123456:AA"+"bcdefghijklmnopqrstuvwxyz01234567")
			cfg, err := config.Load()
			if err != nil {
				t.Fatal(err)
			}
			cfg.CharactersDir = filepath.Join(dir, "characters")
			var built []string
			a, err := New(context.Background(), cfg, "test", Options{
				Model: &fakeModel{}, KeyCheck: func(context.Context, string) error { return nil }, LogOut: io.Discard, NoHTTP: true,
				NewChannel: func(name, token string, _ *slog.Logger) channel.Channel {
					if name == "matrix" && token != tok {
						t.Errorf("matrix built with token %q", token)
					}
					built = append(built, name)
					return &fakeChannel{name: name}
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			got := false
			for _, n := range built {
				got = got || n == "matrix"
			}
			if got != tc.want {
				t.Fatalf("matrix started: %v, want %v (built %v)", got, tc.want, built)
			}
			if len(built) == 0 || built[0] != "telegram" {
				t.Errorf("the other chat apps should start as before: %v", built)
			}
			if v, err := a.Secrets.Get(context.Background(), secrets.MatrixToken); err != nil || v != tok {
				t.Errorf("token not stored: %q %v", v, err)
			}
		})
	}
}
