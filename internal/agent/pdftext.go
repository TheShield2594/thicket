package agent

// A small reader for the text of text-based PDFs, so a local model server
// (which has no PDF parser) can still answer questions about one. It walks
// the page tree, inflates each page's content streams and turns the text
// operators back into characters, through the fonts' ToUnicode maps where
// they have them and WinAnsi (with /Differences) where they don't. There's
// no OCR: a scanned PDF has no text to find, and reading it still needs
// OpenRouter. Every loop is bounded, since the file comes from a chat.

import (
	"bytes"
	"compress/zlib"
	"encoding/ascii85"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

const (
	// maxTextInflate bounds the bytes inflated across one file (content,
	// object and font map streams), so a small PDF can't expand into
	// gigabytes.
	maxTextInflate = 64 << 20
	// maxTextTokens bounds the tokens read across one file.
	maxTextTokens = 20_000_000
	// maxFormDepth bounds forms drawn inside forms.
	maxFormDepth = 6
	// maxCMapEntries bounds one font's code-to-text map.
	maxCMapEntries = 1 << 17
)

var (
	errPDFEncrypted = errors.New("it's encrypted")
	errPDFBudget    = errors.New("too large to read")
)

type (
	pdfName    string
	pdfKeyword string
	pdfDict    map[pdfName]any
	pdfRef     struct{ num, gen int }
	pdfStream  struct {
		dict pdfDict
		raw  []byte
	}
)

// pdfDoc is one file being read.
type pdfDoc struct {
	data    []byte
	offsets map[int]int    // top-level object number → offset after "obj"
	inStm   map[int][2]int // object number → its object stream and index
	cache   map[int]any
	busy    map[int]bool // being resolved: breaks reference cycles
	stmObjs map[int][]any
	fonts   map[any]*pdfFont
	inflate int
	tokens  int
	// short: something wasn't read for running out of budget, so the
	// text is incomplete.
	short bool
}

// pdfText returns the text of a PDF, at most about maxChars bytes of it.
// An empty result with no error means the file has no text layer (it's
// most likely scanned).
func pdfText(b []byte, maxChars int) (string, error) {
	d := &pdfDoc{data: b, offsets: map[int]int{}, inStm: map[int][2]int{}, cache: map[int]any{},
		busy: map[int]bool{}, stmObjs: map[int][]any{}, fonts: map[any]*pdfFont{}, inflate: maxTextInflate, tokens: maxTextTokens}
	d.index()
	if d.encrypted() {
		return "", errPDFEncrypted
	}
	out := &textOut{max: maxChars}
	for _, p := range d.pages() {
		res, _ := d.resolve(p["Resources"]).(pdfDict)
		for _, c := range d.contents(p["Contents"]) {
			if err := d.run(c, res, out, 0); err != nil {
				return out.String(), err // only the budget stops a run
			}
			out.space()
		}
		out.page()
		if out.full() {
			break
		}
	}
	if d.short {
		return out.String(), errPDFBudget
	}
	return out.String(), nil
}

// objRe matches an object header where one can start: at the start of a
// line or right after the previous object, not inside text that only
// shows one.
var objRe = regexp.MustCompile(`(?:\A|[\r\n]|endobj|endstream)[ \t\f\x00]*(\d{1,10})[ \t\r\n\f\x00]+(\d{1,5})[ \t\r\n\f\x00]+obj\b`)

// index finds every top-level object by scanning for "N G obj" rather
// than trusting the cross-reference table, which damaged and edited files
// get wrong. A later definition (an incremental update) wins. Objects
// packed in object streams are indexed too.
func (d *pdfDoc) index() {
	for _, m := range objRe.FindAllSubmatchIndex(d.data, -1) {
		n, err := strconv.Atoi(string(d.data[m[2]:m[3]]))
		if err != nil {
			continue
		}
		d.offsets[n] = m[1]
	}
	nums := make([]int, 0, len(d.offsets))
	for n := range d.offsets {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	for _, n := range nums {
		s, ok := d.object(n).(*pdfStream)
		if !ok || s.dict["Type"] != pdfName("ObjStm") {
			continue
		}
		data, err := d.decode(s)
		if err != nil {
			continue
		}
		count, _ := d.resolve(s.dict["N"]).(float64)
		first, _ := d.resolve(s.dict["First"]).(float64)
		if count <= 0 || count > 1e6 || first <= 0 || int(first) > len(data) {
			continue
		}
		lx := &pdfLexer{b: data[:int(first)], d: d}
		var objs []any
		for i := 0; i < int(count); i++ {
			num, ok1 := lx.next().(float64)
			off, ok2 := lx.next().(float64)
			if !ok1 || !ok2 || off < 0 || off > float64(len(data)-int(first)) {
				break
			}
			body := &pdfLexer{b: data[int(first)+int(off):], d: d}
			objs = append(objs, body.object())
			if _, top := d.offsets[int(num)]; !top {
				d.inStm[int(num)] = [2]int{n, i}
			}
		}
		d.stmObjs[n] = objs
	}
}

// encrypted reports whether the trailer (or cross-reference stream) names
// an encryption dictionary: the strings and streams are then unreadable.
func (d *pdfDoc) encrypted() bool {
	for i := 0; ; {
		j := bytes.Index(d.data[i:], []byte("trailer"))
		if j < 0 {
			break
		}
		i += j + len("trailer")
		lx := &pdfLexer{b: d.data, pos: i, d: d}
		if t, ok := lx.next().(pdfDict); ok && t["Encrypt"] != nil {
			return true
		}
	}
	for n := range d.offsets {
		if s, ok := d.object(n).(*pdfStream); ok && s.dict["Type"] == pdfName("XRef") && s.dict["Encrypt"] != nil {
			return true
		}
	}
	return false
}

// object returns object n, unresolved: a stream, dictionary, or value.
func (d *pdfDoc) object(n int) any {
	if v, ok := d.cache[n]; ok {
		return v
	}
	if d.busy[n] {
		return nil
	}
	d.busy[n] = true
	defer delete(d.busy, n)
	var v any
	if off, ok := d.offsets[n]; ok {
		lx := &pdfLexer{b: d.data, pos: off, d: d}
		v = lx.object()
		if dict, ok := v.(pdfDict); ok {
			if raw, ok := d.streamData(dict, lx.pos); ok {
				v = &pdfStream{dict: dict, raw: raw}
			}
		}
	} else if loc, ok := d.inStm[n]; ok {
		if objs := d.stmObjs[loc[0]]; loc[1] < len(objs) {
			v = objs[loc[1]]
		}
	}
	d.cache[n] = v
	return v
}

// streamData returns the bytes of the stream whose dictionary ends at
// pos, if one follows it.
func (d *pdfDoc) streamData(dict pdfDict, pos int) ([]byte, bool) {
	b := d.data
	for pos < len(b) && isPDFSpace(b[pos]) {
		pos++
	}
	if !bytes.HasPrefix(b[pos:], []byte("stream")) {
		return nil, false
	}
	pos += len("stream")
	if pos < len(b) && b[pos] == '\r' {
		pos++
	}
	if pos < len(b) && b[pos] == '\n' {
		pos++
	}
	// A /Length (direct, or an object of its own) that lands on
	// "endstream" is trusted; otherwise the keyword is searched for.
	if l, ok := d.resolve(dict["Length"]).(float64); ok && l >= 0 && l <= float64(len(b)-pos) && l == math.Trunc(l) {
		end := pos + int(l)
		rest := bytes.TrimLeft(b[end:], " \t\r\n\f\x00")
		if bytes.HasPrefix(rest, []byte("endstream")) {
			return b[pos:end], true
		}
	}
	end := bytes.Index(b[pos:], []byte("endstream"))
	if end < 0 {
		return b[pos:], true
	}
	return bytes.TrimRight(b[pos:pos+end], "\r\n"), true
}

// resolve follows a reference to its object.
func (d *pdfDoc) resolve(v any) any {
	for i := 0; i < 32; i++ {
		r, ok := v.(pdfRef)
		if !ok {
			return v
		}
		v = d.object(r.num)
	}
	return nil
}

// decode returns a stream's data with its filters undone. Only Flate
// (without predictors) is supported, which covers content streams from
// nearly every tool; anything else is an error.
func (d *pdfDoc) decode(s *pdfStream) ([]byte, error) {
	var filters []any
	switch f := d.resolve(s.dict["Filter"]).(type) {
	case nil:
	case pdfName:
		filters = []any{f}
	case []any:
		filters = f
	default:
		return nil, fmt.Errorf("unknown filter")
	}
	data := s.raw
	if len(filters) == 0 {
		// Uncompressed data counts too: a page can name one stream
		// thousands of times.
		if err := d.charge(len(data)); err != nil {
			return nil, err
		}
	}
	for _, f := range filters {
		switch d.resolve(f) {
		case pdfName("FlateDecode"), pdfName("Fl"):
			if parms, ok := d.resolve(s.dict["DecodeParms"]).(pdfDict); ok {
				if p, _ := d.resolve(parms["Predictor"]).(float64); p > 1 {
					return nil, fmt.Errorf("unsupported predictor")
				}
			}
			if d.inflate <= 0 {
				d.short = true
				return nil, errPDFBudget
			}
			r, err := zlib.NewReader(bytes.NewReader(data))
			if err != nil {
				return nil, err
			}
			out, err := io.ReadAll(io.LimitReader(r, int64(d.inflate)+1))
			r.Close()
			if len(out) > d.inflate {
				d.inflate, d.short = 0, true
				return nil, errPDFBudget
			}
			d.inflate -= len(out)
			// A stream cut short still gives what was read of it.
			if err != nil && len(out) == 0 {
				return nil, err
			}
			data = out
		case pdfName("ASCII85Decode"), pdfName("A85"):
			// Decoded, ASCII85 is at most four bytes per character
			// ("z"); that's charged before anything is allocated.
			if err := d.charge(4 * len(data)); err != nil {
				return nil, err
			}
			out, err := decodeASCII85(data)
			if err != nil {
				return nil, err
			}
			data = out
		case pdfName("ASCIIHexDecode"), pdfName("AHx"):
			if err := d.charge(len(data)); err != nil {
				return nil, err
			}
			lx := &pdfLexer{b: append(append([]byte("<"), data...), '>')}
			data = lx.hex()
		default:
			return nil, fmt.Errorf("unsupported filter")
		}
	}
	return data, nil
}

// charge takes n bytes from the file's decoding budget.
func (d *pdfDoc) charge(n int) error {
	if n > d.inflate {
		d.inflate, d.short = 0, true
		return errPDFBudget
	}
	d.inflate -= n
	return nil
}

// decodeASCII85 undoes ASCII85Decode: whitespace is ignored, "z" is four
// zero bytes, and "~>" ends the data.
func decodeASCII85(b []byte) ([]byte, error) {
	b = bytes.TrimPrefix(bytes.TrimLeft(b, " \t\r\n\f\x00"), []byte("<~"))
	if end := bytes.Index(b, []byte("~>")); end >= 0 {
		b = b[:end]
	}
	clean := make([]byte, 0, len(b))
	for _, c := range b {
		if !isPDFSpace(c) {
			clean = append(clean, c)
		}
	}
	out := make([]byte, 4*len(clean)+4) // "z" is one character for four bytes
	n, _, err := ascii85.Decode(out, clean, true)
	if err != nil {
		return nil, err
	}
	return out[:n], nil
}

var rootRe = regexp.MustCompile(`/Root[ \t\r\n\f\x00]+(\d+)[ \t\r\n\f\x00]+\d+[ \t\r\n\f\x00]+R`)

// pages returns the page dictionaries in order, with inherited resources
// filled in. Without a readable page tree, the page objects are taken in
// object order.
func (d *pdfDoc) pages() []pdfDict {
	var out []pdfDict
	seen := map[any]bool{}
	var walk func(node any, res any, depth int)
	walk = func(node any, res any, depth int) {
		if depth > 64 || len(out) > 100_000 {
			return
		}
		key := node
		if _, ok := node.(pdfRef); !ok {
			key = nil
		}
		if key != nil {
			if seen[key] {
				return
			}
			seen[key] = true
		}
		dict, ok := d.resolve(node).(pdfDict)
		if !ok {
			return
		}
		if r, ok := dict["Resources"]; ok {
			res = r
		}
		kids, isTree := d.resolve(dict["Kids"]).([]any)
		if dict["Type"] == pdfName("Pages") || (isTree && dict["Type"] != pdfName("Page")) {
			for _, k := range kids {
				walk(k, res, depth+1)
			}
			return
		}
		page := pdfDict{"Contents": dict["Contents"], "Resources": res}
		out = append(out, page)
	}
	if ms := rootRe.FindAllSubmatch(d.data, -1); len(ms) > 0 {
		n, _ := strconv.Atoi(string(ms[len(ms)-1][1]))
		if cat, ok := d.resolve(pdfRef{num: n}).(pdfDict); ok {
			walk(cat["Pages"], nil, 0)
		}
	}
	if len(out) > 0 {
		return out
	}
	var nums []int
	for n := range d.offsets {
		nums = append(nums, n)
	}
	for n := range d.inStm {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	for _, n := range nums {
		if dict, ok := d.object(n).(pdfDict); ok && dict["Type"] == pdfName("Page") {
			out = append(out, pdfDict{"Contents": dict["Contents"], "Resources": dict["Resources"]})
		}
	}
	return out
}

// contents returns a page's decoded content streams.
func (d *pdfDoc) contents(v any) [][]byte {
	var streams []any
	switch c := d.resolve(v).(type) {
	case *pdfStream:
		streams = []any{c}
	case []any:
		streams = c
	}
	// A page's streams are one content stream split in pieces: a token
	// may run across the join, so they're joined with a space.
	var joined [][]byte
	for _, s := range streams {
		if st, ok := d.resolve(s).(*pdfStream); ok {
			if data, err := d.decode(st); err == nil {
				joined = append(joined, data)
			}
		}
	}
	if len(joined) <= 1 {
		return joined
	}
	return [][]byte{bytes.Join(joined, []byte(" "))}
}

// run interprets a content stream's text operators into out.
func (d *pdfDoc) run(data []byte, res pdfDict, out *textOut, depth int) error {
	lx := &pdfLexer{b: data, d: d, content: true}
	var ops []any
	var font *pdfFont
	var saved []*pdfFont // the font in each graphics state q saved
	lastY, haveY := 0.0, false
	text := func(v any) { out.text(font.decode(v, out.room())) }
	num := func(i int) float64 {
		if i < 0 || i >= len(ops) {
			return 0
		}
		f, _ := ops[i].(float64)
		return f
	}
	for !out.full() {
		if d.tokens <= 0 {
			d.short = true
			return errPDFBudget
		}
		tok := lx.next()
		if tok == nil && lx.pos >= len(lx.b) {
			return nil
		}
		kw, ok := tok.(pdfKeyword)
		if !ok {
			if len(ops) < 64 {
				ops = append(ops, tok)
			}
			continue
		}
		switch kw {
		case "q":
			if len(saved) < 64 {
				saved = append(saved, font)
			}
		case "Q":
			if len(saved) > 0 {
				font, saved = saved[len(saved)-1], saved[:len(saved)-1]
			}
		case "Tf":
			if len(ops) >= 2 {
				name, _ := ops[len(ops)-2].(pdfName)
				font = d.font(res, name)
			}
		case "Tj":
			if len(ops) > 0 {
				text(ops[len(ops)-1])
			}
		case "'", "\"":
			out.newline()
			if len(ops) > 0 {
				text(ops[len(ops)-1])
			}
		case "TJ":
			if len(ops) > 0 {
				arr, _ := ops[len(ops)-1].([]any)
				for _, e := range arr {
					if f, ok := e.(float64); ok {
						// A gap of a quarter em or more is a word break.
						if f < -250 {
							out.space()
						}
						continue
					}
					text(e)
				}
			}
		case "Td", "TD":
			if num(len(ops)-1) != 0 {
				out.newline()
			} else {
				out.space()
			}
		case "T*":
			out.newline()
		case "Tm":
			if len(ops) >= 6 {
				y := num(len(ops) - 1)
				if haveY && y != lastY {
					out.newline()
				} else {
					out.space()
				}
				lastY, haveY = y, true
			}
		case "BT":
			haveY = false
		case "Do":
			if len(ops) > 0 && depth < maxFormDepth {
				name, _ := ops[len(ops)-1].(pdfName)
				xobjs, _ := d.resolve(res["XObject"]).(pdfDict)
				if s, ok := d.resolve(xobjs[name]).(*pdfStream); ok && s.dict["Subtype"] == pdfName("Form") {
					if data, err := d.decode(s); err == nil {
						fres, ok := d.resolve(s.dict["Resources"]).(pdfDict)
						if !ok {
							fres = res
						}
						if err := d.run(data, fres, out, depth+1); err != nil {
							return err
						}
					}
				}
			}
		}
		ops = ops[:0]
	}
	return nil
}

// textOut collects text, with spaces and line breaks kept tidy.
type textOut struct {
	b   strings.Builder
	max int
}

func (o *textOut) full() bool { return o.b.Len() >= o.max }

// room is how many more bytes fit.
func (o *textOut) room() int { return max(o.max-o.b.Len(), 0) }

func (o *textOut) last() byte {
	s := o.b.String()
	if s == "" {
		return '\n'
	}
	return s[len(s)-1]
}

func (o *textOut) text(s string) {
	if s == "" || o.full() {
		return
	}
	o.b.WriteString(s)
}

func (o *textOut) space() {
	if c := o.last(); c != ' ' && c != '\n' {
		o.b.WriteByte(' ')
	}
}

func (o *textOut) newline() {
	s := o.b.String()
	if strings.HasSuffix(s, " ") {
		trimmed := strings.TrimRight(s, " ")
		o.b.Reset()
		o.b.WriteString(trimmed)
	}
	if o.last() != '\n' {
		o.b.WriteByte('\n')
	}
}

func (o *textOut) page() {
	o.newline()
	if !strings.HasSuffix(o.b.String(), "\n\n") && o.b.Len() > 0 {
		o.b.WriteByte('\n')
	}
}

// String returns the text without control characters or blank runs,
// cut at max on a character boundary.
func (o *textOut) String() string {
	s := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r == utf8.RuneError || unicode.IsControl(r) || r == '\uFEFF' {
			return -1
		}
		return r
	}, o.b.String())
	s = strings.TrimSpace(s)
	if len(s) > o.max {
		cut := o.max
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut]
	}
	return s
}

// pdfFont turns a font's character codes into text.
type pdfFont struct {
	cmap    map[string]string // ToUnicode: code bytes → text
	lengths []int             // code lengths in the map, shortest first
	skip    int               // how far an unmapped code advances
	simple  [256]rune         // single-byte fonts without a map
	none    bool              // codes that can't be turned into text
}

// font loads font name from the resources (cached per font object).
func (d *pdfDoc) font(res pdfDict, name pdfName) *pdfFont {
	fonts, _ := d.resolve(res["Font"]).(pdfDict)
	ref := fonts[name]
	key := any(ref)
	if _, ok := ref.(pdfRef); !ok {
		key = nil
	}
	if f, ok := d.fonts[key]; ok && key != nil {
		return f
	}
	dict, _ := d.resolve(ref).(pdfDict)
	f := &pdfFont{}
	if s, ok := d.resolve(dict["ToUnicode"]).(*pdfStream); ok {
		if data, err := d.decode(s); err == nil {
			f.cmap, f.lengths, f.skip = d.parseCMap(data)
		}
	}
	if len(f.cmap) == 0 {
		f.cmap = nil
		if dict["Subtype"] == pdfName("Type0") {
			// Glyph ids with no map back to text: nothing to read.
			f.none = true
		} else {
			f.simple = winAnsi
			d.applyDifferences(f, dict)
		}
	}
	if key != nil {
		d.fonts[key] = f
	}
	return f
}

func (d *pdfDoc) applyDifferences(f *pdfFont, font pdfDict) {
	enc, ok := d.resolve(font["Encoding"]).(pdfDict)
	if !ok {
		return
	}
	diffs, _ := d.resolve(enc["Differences"]).([]any)
	code := -1
	for _, v := range diffs {
		switch v := d.resolve(v).(type) {
		case float64:
			code = int(v)
		case pdfName:
			if code >= 0 && code < 256 {
				if r, ok := glyphRune(string(v)); ok {
					f.simple[code] = r
				}
			}
			code++
		}
	}
}

// decode turns a string operand into text, at most about limit bytes
// of it (the text still fitting in the output).
func (f *pdfFont) decode(v any, limit int) string {
	s, ok := v.([]byte)
	if !ok || limit <= 0 {
		return ""
	}
	if f == nil {
		f = &pdfFont{simple: winAnsi}
	}
	if f.none {
		return ""
	}
	var b strings.Builder
	if f.cmap == nil {
		for _, c := range s {
			if b.Len() >= limit {
				break
			}
			if r := f.simple[c]; r != 0 {
				b.WriteRune(r)
			}
		}
		return b.String()
	}
	for i := 0; i < len(s) && b.Len() < limit; {
		matched := false
		for _, l := range f.lengths {
			if i+l <= len(s) {
				if t, ok := f.cmap[string(s[i:i+l])]; ok {
					b.WriteString(t)
					i += l
					matched = true
					break
				}
			}
		}
		if !matched {
			// An unmapped code: skip it at the code space's width, so
			// it doesn't swallow part of the next code.
			i += f.skip
		}
	}
	return b.String()
}

// maxCMapDest bounds one destination's bytes: real ones are a character
// or a ligature, and a long one repeated over a range would be large.
const maxCMapDest = 32

// parseCMap reads a ToUnicode map's codespace, bfchar and bfrange
// entries: the map, its code lengths (shortest first) and the width an
// unmapped code takes (the codespace's shortest).
func (d *pdfDoc) parseCMap(data []byte) (map[string]string, []int, int) {
	m := map[string]string{}
	lengths := map[int]bool{}
	space := 0
	lx := &pdfLexer{b: data, d: d, content: true}
	var ops []any
	mode := ""
	add := func(code []byte, text string) {
		if len(m) < maxCMapEntries && len(code) > 0 && len(code) <= 4 {
			m[string(code)] = text
			lengths[len(code)] = true
		}
	}
	for d.tokens > 0 {
		tok := lx.next()
		if tok == nil && lx.pos >= len(lx.b) {
			break
		}
		kw, ok := tok.(pdfKeyword)
		if !ok {
			if mode != "" {
				ops = append(ops, tok)
			}
			continue
		}
		switch kw {
		case "beginbfchar", "beginbfrange", "begincodespacerange":
			mode, ops = string(kw), ops[:0]
		case "endcodespacerange":
			for _, o := range ops {
				if b, ok := o.([]byte); ok && len(b) > 0 && len(b) <= 4 && (space == 0 || len(b) < space) {
					space = len(b)
				}
			}
			mode, ops = "", ops[:0]
		case "endbfchar":
			for i := 0; i+1 < len(ops); i += 2 {
				src, ok1 := ops[i].([]byte)
				dst, ok2 := ops[i+1].([]byte)
				if ok1 && ok2 && len(dst) <= maxCMapDest {
					add(src, utf16Text(dst))
				}
			}
			mode, ops = "", ops[:0]
		case "endbfrange":
			for i := 0; i+2 < len(ops); i += 3 {
				lo, ok1 := ops[i].([]byte)
				hi, ok2 := ops[i+1].([]byte)
				if !ok1 || !ok2 || len(lo) != len(hi) || len(lo) == 0 || len(lo) > 4 {
					continue
				}
				a, z := beUint(lo), beUint(hi)
				if z < a || z-a > 0xFFFF {
					continue
				}
				// Counted by offset, so a range ending at the top of the
				// code space can't wrap around and run forever.
				for off := uint32(0); off <= z-a && len(m) < maxCMapEntries; off++ {
					code := beBytes(a+off, len(lo))
					switch dst := ops[i+2].(type) {
					case []byte:
						if len(dst) <= maxCMapDest {
							add(code, utf16Text(bumpUTF16(dst, off)))
						}
					case []any:
						if int(off) < len(dst) {
							if t, ok := dst[off].([]byte); ok && len(t) <= maxCMapDest {
								add(code, utf16Text(t))
							}
						}
					}
				}
			}
			mode, ops = "", ops[:0]
		default:
			if mode == "" {
				ops = ops[:0]
			}
		}
	}
	var ls []int
	for l := range lengths {
		ls = append(ls, l)
	}
	sort.Ints(ls)
	if space == 0 && len(ls) > 0 {
		space = ls[0]
	}
	if space == 0 {
		space = 1
	}
	return m, ls, space
}

func beUint(b []byte) uint32 {
	var n uint32
	for _, c := range b {
		n = n<<8 | uint32(c)
	}
	return n
}

func beBytes(n uint32, l int) []byte {
	b := make([]byte, l)
	for i := l - 1; i >= 0; i-- {
		b[i] = byte(n)
		n >>= 8
	}
	return b
}

// bumpUTF16 adds n to the last UTF-16 unit of b (a bfrange's start).
func bumpUTF16(b []byte, n uint32) []byte {
	if len(b) < 2 {
		return b
	}
	out := append([]byte(nil), b...)
	last := uint32(out[len(out)-2])<<8 | uint32(out[len(out)-1])
	last += n
	out[len(out)-2], out[len(out)-1] = byte(last>>8), byte(last)
	return out
}

// utf16Text decodes UTF-16BE (a ToUnicode destination).
func utf16Text(b []byte) string {
	if len(b) == 1 {
		return string(rune(b[0]))
	}
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u = append(u, uint16(b[i])<<8|uint16(b[i+1]))
	}
	return string(utf16.Decode(u))
}

// pdfLexer reads PDF tokens and objects. In content mode, "R" is an
// operator like any other and inline images are skipped.
type pdfLexer struct {
	b       []byte
	pos     int
	d       *pdfDoc
	content bool
	depth   int
}

func isPDFSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\f' || c == 0
}

func isPDFDelim(c byte) bool {
	switch c {
	case '(', ')', '<', '>', '[', ']', '{', '}', '/', '%':
		return true
	}
	return false
}

// object reads one object, references included.
func (lx *pdfLexer) object() any {
	v := lx.next()
	if n, ok := v.(float64); ok && !lx.content {
		save := lx.pos
		if g, ok := lx.next().(float64); ok {
			if lx.next() == pdfKeyword("R") {
				return pdfRef{num: int(n), gen: int(g)}
			}
		}
		lx.pos = save
	}
	return v
}

// next reads one token: a value (with arrays and dictionaries read
// whole), a keyword, or nil at the end or on a stray delimiter.
func (lx *pdfLexer) next() any {
	if lx.d != nil {
		lx.d.tokens--
		if lx.d.tokens < 0 {
			lx.pos = len(lx.b)
			return nil
		}
	}
	b := lx.b
	for lx.pos < len(b) {
		c := b[lx.pos]
		if isPDFSpace(c) {
			lx.pos++
			continue
		}
		if c == '%' {
			for lx.pos < len(b) && b[lx.pos] != '\n' && b[lx.pos] != '\r' {
				lx.pos++
			}
			continue
		}
		break
	}
	if lx.pos >= len(b) {
		return nil
	}
	c := b[lx.pos]
	switch {
	case c == '/':
		lx.pos++
		var name []byte
		for lx.pos < len(b) && !isPDFSpace(b[lx.pos]) && !isPDFDelim(b[lx.pos]) {
			if b[lx.pos] == '#' && lx.pos+2 < len(b) {
				if v, err := strconv.ParseUint(string(b[lx.pos+1:lx.pos+3]), 16, 8); err == nil {
					name = append(name, byte(v))
					lx.pos += 3
					continue
				}
			}
			name = append(name, b[lx.pos])
			lx.pos++
		}
		return pdfName(name)
	case c == '(':
		return lx.literal()
	case c == '<' && lx.pos+1 < len(b) && b[lx.pos+1] == '<':
		lx.pos += 2
		return lx.dict()
	case c == '<':
		return lx.hex()
	case c == '[':
		lx.pos++
		return lx.array()
	case c == ']' || c == ')' || c == '{' || c == '}' || c == '>':
		lx.pos++
		if c == '>' && lx.pos < len(b) && b[lx.pos] == '>' {
			lx.pos++
		}
		return pdfKeyword(string(c))
	}
	start := lx.pos
	for lx.pos < len(b) && !isPDFSpace(b[lx.pos]) && !isPDFDelim(b[lx.pos]) {
		lx.pos++
	}
	word := string(b[start:lx.pos])
	if f, err := strconv.ParseFloat(word, 64); err == nil {
		return f
	}
	switch word {
	case "true":
		return true
	case "false":
		return false
	case "null":
		return nil
	case "ID":
		if lx.content {
			lx.skipInlineImage()
			return pdfKeyword("EI")
		}
	}
	return pdfKeyword(word)
}

// skipInlineImage moves past an inline image's data, to after its EI.
func (lx *pdfLexer) skipInlineImage() {
	b := lx.b
	for i := lx.pos + 1; i+2 <= len(b); i++ {
		if b[i] == 'E' && b[i+1] == 'I' && isPDFSpace(b[i-1]) && (i+2 == len(b) || isPDFSpace(b[i+2]) || isPDFDelim(b[i+2])) {
			lx.pos = i + 2
			return
		}
	}
	lx.pos = len(b)
}

func (lx *pdfLexer) array() []any {
	var out []any
	lx.depth++
	defer func() { lx.depth-- }()
	if lx.depth > 64 {
		lx.pos = len(lx.b)
		return nil
	}
	for lx.pos < len(lx.b) {
		v := lx.object()
		if v == pdfKeyword("]") {
			return out
		}
		if v == nil && lx.pos >= len(lx.b) {
			return out
		}
		if len(out) < 1<<16 {
			out = append(out, v)
		}
	}
	return out
}

func (lx *pdfLexer) dict() pdfDict {
	out := pdfDict{}
	lx.depth++
	defer func() { lx.depth-- }()
	if lx.depth > 64 {
		lx.pos = len(lx.b)
		return out
	}
	for lx.pos < len(lx.b) {
		k := lx.next()
		if k == pdfKeyword(">") || (k == nil && lx.pos >= len(lx.b)) {
			return out
		}
		name, ok := k.(pdfName)
		if !ok {
			continue
		}
		v := lx.object()
		if v == pdfKeyword(">") {
			return out
		}
		out[name] = v
	}
	return out
}

func (lx *pdfLexer) literal() []byte {
	b := lx.b
	lx.pos++ // (
	var out []byte
	depth := 1
	for lx.pos < len(b) {
		c := b[lx.pos]
		lx.pos++
		switch c {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return out
			}
		case '\\':
			if lx.pos >= len(b) {
				return out
			}
			e := b[lx.pos]
			lx.pos++
			switch e {
			case 'n':
				c = '\n'
			case 'r':
				c = '\r'
			case 't':
				c = '\t'
			case 'b':
				c = '\b'
			case 'f':
				c = '\f'
			case '\r':
				if lx.pos < len(b) && b[lx.pos] == '\n' {
					lx.pos++
				}
				continue
			case '\n':
				continue
			default:
				if e >= '0' && e <= '7' {
					v := int(e - '0')
					for k := 0; k < 2 && lx.pos < len(b) && b[lx.pos] >= '0' && b[lx.pos] <= '7'; k++ {
						v = v*8 + int(b[lx.pos]-'0')
						lx.pos++
					}
					c = byte(v)
				} else {
					c = e
				}
			}
		}
		out = append(out, c)
	}
	return out
}

func (lx *pdfLexer) hex() []byte {
	b := lx.b
	lx.pos++ // <
	var out []byte
	hi, half := byte(0), false
	for lx.pos < len(b) {
		c := b[lx.pos]
		lx.pos++
		var v byte
		switch {
		case c == '>':
			if half {
				out = append(out, hi<<4)
			}
			return out
		case c >= '0' && c <= '9':
			v = c - '0'
		case c >= 'a' && c <= 'f':
			v = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			v = c - 'A' + 10
		default:
			continue
		}
		if half {
			out = append(out, hi<<4|v)
		} else {
			hi = v
		}
		half = !half
	}
	return out
}

// winAnsi is WinAnsiEncoding: Latin-1 with the 0x80–0x9F block filled.
var winAnsi = func() [256]rune {
	var t [256]rune
	for i := 0x20; i < 256; i++ {
		t[i] = rune(i)
	}
	t['\t'], t['\n'], t['\r'] = ' ', ' ', ' '
	for i, r := range []rune{'€', 0, '‚', 'ƒ', '„', '…', '†', '‡', 'ˆ', '‰', 'Š', '‹', 'Œ', 0, 'Ž', 0,
		0, '‘', '’', '“', '”', '•', '–', '—', '˜', '™', 'š', '›', 'œ', 0, 'ž', 'Ÿ'} {
		t[0x80+i] = r
	}
	t[0x7F], t[0xAD] = 0, '-'
	return t
}()

// glyphNames covers the common glyph names in /Differences beyond single
// letters and digits' names.
var glyphNames = map[string]rune{
	"space": ' ', "exclam": '!', "quotedbl": '"', "numbersign": '#', "dollar": '$', "percent": '%',
	"ampersand": '&', "quotesingle": '\'', "quoteright": '’', "quoteleft": '‘', "parenleft": '(',
	"parenright": ')', "asterisk": '*', "plus": '+', "comma": ',', "hyphen": '-', "minus": '−',
	"period": '.', "slash": '/', "colon": ':', "semicolon": ';', "less": '<', "equal": '=',
	"greater": '>', "question": '?', "at": '@', "bracketleft": '[', "backslash": '\\',
	"bracketright": ']', "asciicircum": '^', "underscore": '_', "grave": '`', "braceleft": '{',
	"bar": '|', "braceright": '}', "asciitilde": '~', "bullet": '•', "endash": '–', "emdash": '—',
	"quotedblleft": '“', "quotedblright": '”', "quotesinglbase": '‚', "quotedblbase": '„',
	"ellipsis": '…', "fi": 'ﬁ', "fl": 'ﬂ', "ff": 'ﬀ', "ffi": 'ﬃ', "ffl": 'ﬄ', "dagger": '†',
	"daggerdbl": '‡', "degree": '°', "copyright": '©', "registered": '®', "trademark": '™',
	"section": '§', "paragraph": '¶', "Euro": '€', "sterling": '£', "yen": '¥', "cent": '¢',
	"germandbls": 'ß', "dotlessi": 'ı', "periodcentered": '·', "multiply": '×', "divide": '÷',
	"zero": '0', "one": '1', "two": '2', "three": '3', "four": '4', "five": '5', "six": '6',
	"seven": '7', "eight": '8', "nine": '9', "nbspace": ' ', "sfthyphen": '-',
}

func glyphRune(name string) (rune, bool) {
	if r, ok := glyphNames[name]; ok {
		return r, true
	}
	if len(name) == 1 {
		return rune(name[0]), true
	}
	if strings.HasPrefix(name, "uni") && len(name) == 7 {
		if v, err := strconv.ParseUint(name[3:], 16, 32); err == nil {
			return rune(v), true
		}
	}
	// Accented Latin letters: "eacute" → é and so on.
	for _, acc := range []struct {
		suffix string
		mark   rune
	}{{"acute", '́'}, {"grave", '̀'}, {"circumflex", '̂'}, {"dieresis", '̈'}, {"tilde", '̃'}, {"cedilla", '̧'}, {"ring", '̊'}} {
		if base, ok := strings.CutSuffix(name, acc.suffix); ok && len(base) == 1 {
			return composeLatin(rune(base[0]), acc.mark), true
		}
	}
	return 0, false
}

// composeLatin returns base with mark composed, for the Latin-1 letters.
func composeLatin(base, mark rune) rune {
	for r := rune(0xC0); r <= 0xFF; r++ {
		if decomp, ok := latinDecomp[r]; ok && decomp == [2]rune{base, mark} {
			return r
		}
	}
	return base
}

var latinDecomp = func() map[rune][2]rune {
	m := map[rune][2]rune{}
	bases := map[rune]string{'A': "ÀÁÂÃÄÅ", 'a': "àáâãäå", 'E': "ÈÉÊË", 'e': "èéêë", 'I': "ÌÍÎÏ", 'i': "ìíîï",
		'O': "ÒÓÔÕÖ", 'o': "òóôõö", 'U': "ÙÚÛÜ", 'u': "ùúûü", 'N': "Ñ", 'n': "ñ", 'C': "Ç", 'c': "ç", 'Y': "Ý", 'y': "ýÿ"}
	marks := map[rune]rune{'À': '̀', 'Á': '́', 'Â': '̂', 'Ã': '̃', 'Ä': '̈', 'Å': '̊',
		'à': '̀', 'á': '́', 'â': '̂', 'ã': '̃', 'ä': '̈', 'å': '̊',
		'È': '̀', 'É': '́', 'Ê': '̂', 'Ë': '̈', 'è': '̀', 'é': '́', 'ê': '̂', 'ë': '̈',
		'Ì': '̀', 'Í': '́', 'Î': '̂', 'Ï': '̈', 'ì': '̀', 'í': '́', 'î': '̂', 'ï': '̈',
		'Ò': '̀', 'Ó': '́', 'Ô': '̂', 'Õ': '̃', 'Ö': '̈', 'ò': '̀', 'ó': '́', 'ô': '̂', 'õ': '̃', 'ö': '̈',
		'Ù': '̀', 'Ú': '́', 'Û': '̂', 'Ü': '̈', 'ù': '̀', 'ú': '́', 'û': '̂', 'ü': '̈',
		'Ñ': '̃', 'ñ': '̃', 'Ç': '̧', 'ç': '̧', 'Ý': '́', 'ý': '́', 'ÿ': '̈'}
	for base, rs := range bases {
		for _, r := range rs {
			m[r] = [2]rune{base, marks[r]}
		}
	}
	return m
}()
