package avatar

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEveryCombinationIsWellFormed(t *testing.T) {
	for _, body := range Bodies {
		for _, hat := range Hats {
			for _, acc := range Accessories {
				for _, ex := range Expressions {
					p := Params{Body: body, Color: "#5aa469", Hat: hat, HatColor: "#3f7d4e", Accessory: acc, Expression: ex}
					if err := p.Validate(); err != nil {
						t.Fatal(err)
					}
					for _, st := range States {
						svg := Render(p, st)
						if err := xml.Unmarshal([]byte(svg), new(struct{})); err != nil {
							t.Fatalf("%+v %s: not well-formed XML: %v", p, st, err)
						}
						if !strings.Contains(svg, `class="sprig is-`+st+`"`) {
							t.Fatalf("state class missing for %s", st)
						}
					}
				}
			}
		}
	}
}

func TestParamsAreChecked(t *testing.T) {
	bad := Default("pip", "")
	bad.Color = `#fff" onload="alert(1)`
	if bad.Validate() == nil {
		t.Fatal("injected color accepted")
	}
	// Bad parameters never reach the SVG.
	if svg := Render(bad, "idle"); strings.Contains(svg, "onload") {
		t.Fatal("injection drawn")
	}
	if svg := Inline(Default("pip", ""), `idle"><script>`, `x" onclick="y`, `<b>Pip</b>`); strings.Contains(svg, "<script>") || strings.Contains(svg, "onclick") || strings.Contains(svg, "<b>") {
		t.Fatalf("state, id or title not escaped: %s", svg)
	}
	if p := Default("pip", "#123456"); p.Color != "#123456" || p.Validate() != nil {
		t.Fatalf("default: %+v", p)
	}
	if Default("pip", "") != Default("pip", "") || Default("pip", "") == Default("juniper", "") {
		t.Fatal("defaults must be stable per id and differ between ids")
	}
}

func TestLoadFallsBackToDefault(t *testing.T) {
	dir := t.TempDir()
	if p := Load(dir, "pip", "#123456"); p != Default("pip", "#123456") {
		t.Fatal("missing file should give the default")
	}
	os.MkdirAll(filepath.Join(dir, "pip"), 0o755)
	os.WriteFile(Path(dir, "pip"), []byte(`{"hat":"fern","expression":"curious"}`), 0o644)
	p := Load(dir, "pip", "#123456")
	if p.Hat != "fern" || p.Expression != "curious" || p.Color != "#123456" {
		t.Fatalf("partial file not merged: %+v", p)
	}
	os.WriteFile(Path(dir, "pip"), []byte(`{"hat":"crown"}`), 0o644)
	if p := Load(dir, "pip", ""); p.Hat == "crown" {
		t.Fatal("invalid part accepted")
	}
}

// AVATAR_SHEET=/path/sheet.html go test ./internal/avatar -run Sheet
// writes a page with every part and state, for looking at.
func TestContactSheet(t *testing.T) {
	out := os.Getenv("AVATAR_SHEET")
	if out == "" {
		t.Skip("set AVATAR_SHEET to write a contact sheet")
	}
	var b strings.Builder
	b.WriteString("<!doctype html><meta charset=utf-8><style>" + CSS + "body{font:13px sans-serif;background:#f7f6f2}div{display:inline-block;text-align:center;margin:6px}</style>")
	row := func(title string, ps []Params, state string) {
		b.WriteString("<h3>" + title + "</h3>")
		for _, p := range ps {
			b.WriteString("<div>" + Inline(p, state, "", "") + "<br>" + p.Body + " " + p.Hat + " " + p.Accessory + " " + p.Expression + "</div>")
		}
	}
	base := Params{Body: "round", Color: "#5aa469", Hat: "curl", HatColor: "#3f7d4e", Accessory: "watering-can", Expression: "happy"}
	var ps []Params
	for i, body := range Bodies {
		p := base
		p.Body, p.Hat, p.Color = body, Hats[i], palette[i+2]
		ps = append(ps, p)
	}
	row("bodies and hats", ps, "idle")
	ps = nil
	for i, acc := range Accessories {
		p := base
		p.Accessory, p.Expression = acc, Expressions[i%len(Expressions)]
		ps = append(ps, p)
	}
	row("accessories and expressions", ps, "idle")
	for _, st := range States {
		row("state: "+st, []Params{base, Default("pip", ""), Default("hal", "")}, st)
	}
	if err := os.WriteFile(out, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}
