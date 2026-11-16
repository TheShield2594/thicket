// Package avatar draws sprigs as small parametric SVGs, built from parts
// (body shape and colour, leaf hat, accessory, expression), with CSS
// animations for what a sprig is doing: idle, thinking, working, sleeping,
// stuck. No image generation: the same parameters always draw the same
// sprig. Parameters live in <characters dir>/<id>/avatar.json.
package avatar

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// The parts to choose from.
var (
	Bodies      = []string{"round", "tall", "pear", "bean"}
	Hats        = []string{"sprout", "curl", "clover", "fern", "none"}
	Accessories = []string{"none", "watering-can", "glasses", "scarf", "book", "lantern"}
	Expressions = []string{"happy", "calm", "curious", "sleepy", "determined"}
	States      = []string{"idle", "thinking", "working", "sleeping", "stuck"}
)

// Params describe one sprig's look.
type Params struct {
	Body       string `json:"body"`
	Color      string `json:"color"`
	Hat        string `json:"hat"`
	HatColor   string `json:"hat_color"`
	Accessory  string `json:"accessory"`
	Expression string `json:"expression"`
}

var colorRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func oneOf(v string, set []string) bool {
	for _, s := range set {
		if v == s {
			return true
		}
	}
	return false
}

// Validate checks every part against the known sets. Parameters are drawn
// straight into SVG, so nothing else is accepted.
func (p Params) Validate() error {
	var probs []string
	if !oneOf(p.Body, Bodies) {
		probs = append(probs, "body must be one of "+strings.Join(Bodies, ", "))
	}
	if !oneOf(p.Hat, Hats) {
		probs = append(probs, "hat must be one of "+strings.Join(Hats, ", "))
	}
	if !oneOf(p.Accessory, Accessories) {
		probs = append(probs, "accessory must be one of "+strings.Join(Accessories, ", "))
	}
	if !oneOf(p.Expression, Expressions) {
		probs = append(probs, "expression must be one of "+strings.Join(Expressions, ", "))
	}
	if !colorRe.MatchString(p.Color) || !colorRe.MatchString(p.HatColor) {
		probs = append(probs, "colors must look like #5aa469")
	}
	if len(probs) > 0 {
		return errors.New(strings.Join(probs, "; "))
	}
	return nil
}

var palette = []string{"#5aa469", "#7a9e7e", "#d98c5f", "#6c8ebf", "#b07cc6", "#d4b24c", "#cf6f7d", "#4fa3a5"}
var leafs = []string{"#3f7d4e", "#4f9a58", "#6aa84f", "#2f6f5e"}

// Default picks a look from the sprig's id, so every sprig has one even
// before anyone chooses. color (the character's) is used when valid.
func Default(id, color string) Params {
	h := fnv.New32a()
	h.Write([]byte(id))
	n := h.Sum32()
	pick := func(set []string, shift uint) string { return set[int(n>>shift)%len(set)] }
	p := Params{Body: pick(Bodies, 0), Hat: pick(Hats[:4], 3), HatColor: pick(leafs, 6),
		Accessory: pick(Accessories, 9), Expression: pick(Expressions, 12), Color: pick(palette, 15)}
	if colorRe.MatchString(color) {
		p.Color = color
	}
	return p
}

// Merge fills the empty fields of p from def.
func (p Params) Merge(def Params) Params {
	if p.Body == "" {
		p.Body = def.Body
	}
	if p.Color == "" {
		p.Color = def.Color
	}
	if p.Hat == "" {
		p.Hat = def.Hat
	}
	if p.HatColor == "" {
		p.HatColor = def.HatColor
	}
	if p.Accessory == "" {
		p.Accessory = def.Accessory
	}
	if p.Expression == "" {
		p.Expression = def.Expression
	}
	return p
}

// Path is where a sprig's avatar parameters are kept.
func Path(charsDir, id string) string { return filepath.Join(charsDir, id, "avatar.json") }

// Load reads a sprig's parameters, filling gaps (or a missing or broken
// file) from Default.
func Load(charsDir, id, color string) Params {
	def := Default(id, color)
	b, err := os.ReadFile(Path(charsDir, id))
	if err != nil {
		return def
	}
	var p Params
	if json.Unmarshal(b, &p) != nil {
		return def
	}
	p = p.Merge(def)
	if p.Validate() != nil {
		return def
	}
	return p
}

// Marshal is the avatar.json form of p.
func Marshal(p Params) []byte {
	b, _ := json.MarshalIndent(p, "", "  ")
	return append(b, '\n')
}

// shade darkens (f < 1) or lightens (f > 1) a #rrggbb color.
func shade(hex string, f float64) string {
	v, _ := strconv.ParseUint(strings.TrimPrefix(hex, "#"), 16, 32)
	c := func(x uint64) uint64 {
		y := float64(x) * f
		if f > 1 {
			y = float64(x) + (255-float64(x))*(f-1)
		}
		if y > 255 {
			y = 255
		}
		return uint64(y)
	}
	return fmt.Sprintf("#%02x%02x%02x", c(v>>16&255), c(v>>8&255), c(v&255))
}

// CSS animates the states. A page with several inline avatars includes it
// once; Render (standalone) embeds it.
const CSS = `.sprig .fig{transform-origin:60px 108px;animation:sprig-bob 3.2s ease-in-out infinite}
.sprig .closed,.sprig .think,.sprig .spark,.sprig .zzz,.sprig .alert{display:none}
.sprig.is-thinking .fig{animation:sprig-tilt 2.6s ease-in-out infinite}
.sprig.is-thinking .think{display:inline}
.sprig .think circle{animation:sprig-blink 1.2s ease-in-out infinite}
.sprig .think circle:nth-child(2){animation-delay:.2s}.sprig .think circle:nth-child(3){animation-delay:.4s}
.sprig.is-working .fig{animation:sprig-hop .7s ease-in-out infinite}
.sprig.is-working .spark{display:inline;animation:sprig-blink .9s ease-in-out infinite}
.sprig.is-sleeping .fig{animation:sprig-breathe 4.5s ease-in-out infinite;opacity:.85}
.sprig.is-sleeping .open{display:none}.sprig.is-sleeping .closed{display:inline}
.sprig.is-sleeping .zzz{display:inline;animation:sprig-float 3s ease-in-out infinite}
.sprig.is-stuck .fig{animation:sprig-shake 2.4s ease-in-out infinite}
.sprig.is-stuck .alert{display:inline;animation:sprig-blink 1.4s ease-in-out infinite}
@keyframes sprig-bob{0%,100%{transform:translateY(0)}50%{transform:translateY(-2px)}}
@keyframes sprig-tilt{0%,100%{transform:rotate(0)}30%{transform:rotate(-5deg)}60%{transform:rotate(3deg)}}
@keyframes sprig-hop{0%,100%{transform:translateY(0)}50%{transform:translateY(-6px) scaleY(1.03)}}
@keyframes sprig-breathe{0%,100%{transform:scale(1)}50%{transform:scale(1.025,.985)}}
@keyframes sprig-shake{0%,20%,100%{transform:translateX(0)}4%,12%{transform:translateX(-3px)}8%,16%{transform:translateX(3px)}}
@keyframes sprig-blink{0%,100%{opacity:.25}50%{opacity:1}}
@keyframes sprig-float{0%{transform:translate(0,4px);opacity:0}40%{opacity:1}100%{transform:translate(6px,-8px);opacity:0}}
@media (prefers-reduced-motion:reduce){.sprig *{animation:none!important}}`

// Render draws a standalone SVG (styles included) in a state.
func Render(p Params, state string) string {
	return draw(p, state, "<style>"+CSS+"</style>", "")
}

// Inline draws an SVG for embedding in a page that includes CSS once. id
// becomes the element id (for live state updates); title is its label.
func Inline(p Params, state, id, title string) string {
	return draw(p, state, "<title>"+escape(title)+"</title>", id)
}

func escape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")
	return r.Replace(s)
}

var idRe = regexp.MustCompile(`^[a-z0-9-]{0,64}$`)

func draw(p Params, state, head, id string) string {
	if p.Validate() != nil {
		p = Default("sprig", "")
	}
	if !oneOf(state, States) {
		state = "idle"
	}
	attrID := ""
	if id != "" && idRe.MatchString(id) {
		attrID = ` id="` + id + `"`
	}
	stroke := shade(p.Color, 0.7)
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 120 120" width="120" height="120" class="sprig is-%s"%s role="img">%s`, state, attrID, head)
	b.WriteString(`<ellipse cx="60" cy="110" rx="26" ry="4" fill="#000" opacity=".12"/>`)
	b.WriteString(`<g class="fig">`)
	// Body; top is where the hat sits, eyeY where the face is.
	top, eyeY := 40.0, 70.0
	fill := fmt.Sprintf(`fill="%s" stroke="%s" stroke-width="2.5"`, p.Color, stroke)
	switch p.Body {
	case "round":
		fmt.Fprintf(&b, `<circle cx="60" cy="74" r="33" %s/>`, fill)
	case "tall":
		fmt.Fprintf(&b, `<rect x="33" y="38" width="54" height="70" rx="26" %s/>`, fill)
		top, eyeY = 39, 64
	case "pear":
		fmt.Fprintf(&b, `<path d="M60 40c13 0 18 12 18 20 0 6 14 14 14 28 0 13-14 20-32 20s-32-7-32-20c0-14 14-22 14-28 0-8 5-20 18-20z" %s/>`, fill)
		top, eyeY = 41, 66
	case "bean":
		fmt.Fprintf(&b, `<ellipse cx="60" cy="78" rx="37" ry="29" %s/>`, fill)
		top, eyeY = 50, 74
	}
	// A soft highlight.
	fmt.Fprintf(&b, `<ellipse cx="48" cy="%.0f" rx="9" ry="5" fill="#fff" opacity=".18"/>`, eyeY-14)
	// Hat.
	leaf, vein := p.HatColor, shade(p.HatColor, 0.7)
	switch p.Hat {
	case "sprout":
		fmt.Fprintf(&b, `<path d="M60 %[1]f v-10" stroke="%[3]s" stroke-width="2.5" fill="none"/><path d="M60 %[2]f c-4-9-15-10-19-6 4 6 12 9 19 6z" fill="%[4]s" stroke="%[3]s" stroke-width="1.5"/><path d="M60 %[2]f c4-11 16-12 20-7-4 7-13 10-20 7z" fill="%[4]s" stroke="%[3]s" stroke-width="1.5"/>`, top, top-9, vein, leaf)
	case "curl":
		fmt.Fprintf(&b, `<path d="M58 %[1]f c-2-10 6-18 15-16 7 2 8 10 3 12-4 2-7-1-5-4" fill="none" stroke="%[2]s" stroke-width="2.5" stroke-linecap="round"/><path d="M58 %[1]f c-6-4-14-3-17 2 6 3 13 2 17-2z" fill="%[3]s" stroke="%[2]s" stroke-width="1.5"/>`, top+1, vein, leaf)
	case "clover":
		fmt.Fprintf(&b, `<path d="M60 %[1]f v-8" stroke="%[2]s" stroke-width="2.5"/><circle cx="54" cy="%[4]f" r="6" fill="%[3]s" stroke="%[2]s" stroke-width="1.5"/><circle cx="66" cy="%[4]f" r="6" fill="%[3]s" stroke="%[2]s" stroke-width="1.5"/><circle cx="60" cy="%[5]f" r="6" fill="%[3]s" stroke="%[2]s" stroke-width="1.5"/>`, top, vein, leaf, top-10, top-17)
	case "fern":
		fmt.Fprintf(&b, `<path d="M60 %[1]f q2-14 12-22" fill="none" stroke="%[2]s" stroke-width="2.5" stroke-linecap="round"/>`, top, vein)
		for i, d := range []float64{4, 9, 14, 19} {
			x, y := 60+d*0.45, top-d*0.95
			r := 5.0 - float64(i)*0.7
			fmt.Fprintf(&b, `<ellipse cx="%.1f" cy="%.1f" rx="%.1f" ry="2.4" transform="rotate(-35 %.1f %.1f)" fill="%s"/><ellipse cx="%.1f" cy="%.1f" rx="%.1f" ry="2.4" transform="rotate(35 %.1f %.1f)" fill="%s"/>`,
				x-4, y, r, x-4, y, leaf, x+4, y-2, r, x+4, y-2, leaf)
		}
	}
	// Face: open eyes per expression, closed eyes for sleeping, mouth.
	ink := "#24302a"
	b.WriteString(`<g class="open">`)
	switch p.Expression {
	case "happy":
		fmt.Fprintf(&b, `<path d="M45 %[1]f q4-5 8 0M67 %[1]f q4-5 8 0" fill="none" stroke="%[2]s" stroke-width="2.5" stroke-linecap="round"/>`, eyeY, ink)
	case "calm":
		fmt.Fprintf(&b, `<circle cx="49" cy="%[1]f" r="3" fill="%[2]s"/><circle cx="71" cy="%[1]f" r="3" fill="%[2]s"/>`, eyeY, ink)
	case "curious":
		fmt.Fprintf(&b, `<circle cx="49" cy="%[1]f" r="3" fill="%[2]s"/><circle cx="71" cy="%[1]f" r="4.5" fill="%[2]s"/><circle cx="72.5" cy="%[3]f" r="1.3" fill="#fff"/>`, eyeY, ink, eyeY-1.5)
	case "sleepy":
		fmt.Fprintf(&b, `<path d="M45 %[1]f h8M67 %[1]f h8" stroke="%[2]s" stroke-width="2.5" stroke-linecap="round"/><path d="M45 %[3]f q4 3 8 0M67 %[3]f q4 3 8 0" fill="none" stroke="%[2]s" stroke-width="1.5"/>`, eyeY-1, ink, eyeY+1)
	case "determined":
		fmt.Fprintf(&b, `<circle cx="49" cy="%[1]f" r="3" fill="%[2]s"/><circle cx="71" cy="%[1]f" r="3" fill="%[2]s"/><path d="M44 %[3]f l9 3M76 %[3]f l-9 3" stroke="%[2]s" stroke-width="2.2" stroke-linecap="round"/>`, eyeY, ink, eyeY-8)
	}
	b.WriteString(`</g>`)
	fmt.Fprintf(&b, `<g class="closed"><path d="M45 %[1]f q4 3 8 0M67 %[1]f q4 3 8 0" fill="none" stroke="%[2]s" stroke-width="2.5" stroke-linecap="round"/></g>`, eyeY, ink)
	fmt.Fprintf(&b, `<circle cx="42" cy="%[1]f" r="4" fill="#f29a9a" opacity=".45"/><circle cx="78" cy="%[1]f" r="4" fill="#f29a9a" opacity=".45"/>`, eyeY+7)
	mouthY := eyeY + 10
	switch p.Expression {
	case "curious":
		fmt.Fprintf(&b, `<circle cx="60" cy="%.0f" r="2.5" fill="none" stroke="%s" stroke-width="2"/>`, mouthY, ink)
	case "determined":
		fmt.Fprintf(&b, `<path d="M55 %[1]f h10" stroke="%[2]s" stroke-width="2.2" stroke-linecap="round"/>`, mouthY, ink)
	default:
		fmt.Fprintf(&b, `<path d="M54 %[1]f q6 5 12 0" fill="none" stroke="%[2]s" stroke-width="2.2" stroke-linecap="round"/>`, mouthY-1, ink)
	}
	// Accessory.
	switch p.Accessory {
	case "glasses":
		fmt.Fprintf(&b, `<g fill="none" stroke="%[2]s" stroke-width="2"><circle cx="49" cy="%[1]f" r="7"/><circle cx="71" cy="%[1]f" r="7"/><path d="M56 %[1]f h8"/></g>`, eyeY, ink)
	case "scarf":
		fmt.Fprintf(&b, `<path d="M36 %[1]f q24 10 48 0 v7 q-24 10-48 0z" fill="#d9534f" stroke="#a33c38" stroke-width="1.5"/><path d="M72 %[2]f l4 14 6-2-3-13z" fill="#d9534f" stroke="#a33c38" stroke-width="1.5"/>`, eyeY+17, eyeY+22)
	case "watering-can":
		b.WriteString(`<g transform="translate(84 80)"><rect x="0" y="4" width="16" height="13" rx="3" fill="#8fb3c9" stroke="#5b7f95" stroke-width="1.5"/><path d="M16 8 l9-7" stroke="#5b7f95" stroke-width="2.5" stroke-linecap="round"/><path d="M3 4 q5-7 10 0" fill="none" stroke="#5b7f95" stroke-width="2"/></g>`)
	case "book":
		b.WriteString(`<g transform="translate(16 82)"><rect x="0" y="0" width="18" height="14" rx="2" fill="#c8553d" stroke="#8c3a29" stroke-width="1.5"/><path d="M9 0 v14" stroke="#8c3a29" stroke-width="1.2"/></g>`)
	case "lantern":
		b.WriteString(`<g transform="translate(86 74)"><path d="M6 0 v4" stroke="#6b5a2e" stroke-width="2"/><rect x="0" y="4" width="12" height="16" rx="3" fill="#f6d36b" stroke="#a88b2c" stroke-width="1.5"/><circle cx="6" cy="12" r="3" fill="#fff6c9"/></g>`)
	}
	b.WriteString(`</g>`) // fig
	// State decorations.
	b.WriteString(`<g class="think" fill="#7d8a83"><circle cx="88" cy="26" r="3"/><circle cx="97" cy="20" r="3.5"/><circle cx="107" cy="13" r="4"/></g>`)
	b.WriteString(`<g class="spark" fill="#f2c94c"><path d="M20 30l2 6 6 2-6 2-2 6-2-6-6-2 6-2z"/><path d="M98 36l1.5 4 4 1.5-4 1.5-1.5 4-1.5-4-4-1.5 4-1.5z"/></g>`)
	b.WriteString(`<g class="zzz" fill="#7d8a83" font-family="sans-serif" font-weight="700"><text x="86" y="34" font-size="12">z</text><text x="96" y="22" font-size="15">z</text></g>`)
	b.WriteString(`<g class="alert"><circle cx="98" cy="24" r="10" fill="#d9534f"/><path d="M98 17v9" stroke="#fff" stroke-width="3" stroke-linecap="round"/><circle cx="98" cy="30.5" r="1.8" fill="#fff"/></g>`)
	b.WriteString(`</svg>`)
	return b.String()
}
