package plugin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestResolve(t *testing.T) {
	s := Spec{Name: "voice", DefaultURL: "http://voice:7071/"}
	if s.URLEnv() != "THICKET_VOICE_URL" || s.TokenFileEnv() != "THICKET_VOICE_TOKEN_FILE" || s.Secret() != "voice_token" || s.Scope() != "mcp:voice/*" {
		t.Fatalf("names: %s %s %s %s", s.URLEnv(), s.TokenFileEnv(), s.Secret(), s.Scope())
	}
	for name, c := range map[string]struct {
		env      map[string]string
		url      string
		explicit bool
	}{
		"unset uses the default quietly": {nil, "http://voice:7071", false},
		"set is explicit":                {map[string]string{"THICKET_VOICE_URL": "http://box:1/"}, "http://box:1", true},
		"empty turns it off":             {map[string]string{"THICKET_VOICE_URL": ""}, "", false},
		"off turns it off":               {map[string]string{"THICKET_VOICE_URL": "OFF"}, "", false},
	} {
		ep := s.Resolve(env(c.env), "/data")
		if ep.URL != c.url || ep.Explicit != c.explicit {
			t.Errorf("%s: %+v", name, ep)
		}
		if ep.TokenFile != filepath.Join("/data", "voice", "token") {
			t.Errorf("%s: token file %s", name, ep.TokenFile)
		}
	}
	// The sandbox has no default address: off unless set.
	if ep := Sandbox.Resolve(env(nil), "/d"); ep.URL != "" {
		t.Fatalf("sandbox on without a URL: %+v", ep)
	}
	ep := Sandbox.Resolve(env(map[string]string{"THICKET_SANDBOX_URL": "http://sandbox:7070", "THICKET_SANDBOX_TOKEN_FILE": "/sandbox-auth/token"}), "/d")
	if ep.MCPURL() != "http://sandbox:7070/mcp" || ep.TokenFile != "/sandbox-auth/token" || !ep.Explicit || !ep.Exec {
		t.Fatalf("sandbox: %+v", ep)
	}
}

func TestRegistry(t *testing.T) {
	for _, bad := range [][]Spec{{{Name: "Bad"}}, {{Name: "a"}}, {{Name: "has-dash"}}, {{Name: "dup"}, {Name: "dup"}}} {
		if _, err := New(bad...); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	r := MustNew(Spec{Name: "browser"}, Spec{Name: "voice", KidsAllowed: true})
	if _, ok := r.Get("browser"); !ok {
		t.Fatal("no browser")
	}
	if err := r.KidRefused([]string{"web", "mcp:ha/get_state", "mcp:voice/speak"}); err != nil {
		t.Fatal(err)
	}
	for _, sc := range []string{"mcp:browser/*", "mcp:browser/open_page", "mcp:browser"} {
		if r.KidRefused([]string{sc}) == nil {
			t.Errorf("%s allowed for kids", sc)
		}
	}
	if Builtin().KidRefused([]string{"mcp:sandbox/read_file"}) == nil {
		t.Fatal("kids may use the sandbox")
	}
	if ScopeServer("web") != "" || ScopeServer("mcp:x/y") != "x" {
		t.Fatal("ScopeServer")
	}
}

func TestAnswers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) }))
	defer srv.Close()
	if !Answers(context.Background(), srv.URL) {
		t.Fatal("a server answering 404 doesn't answer")
	}
	srv.Close()
	if Answers(context.Background(), srv.URL) {
		t.Fatal("a closed server answers")
	}
}
