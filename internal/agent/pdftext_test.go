package agent

import (
	"bytes"
	"compress/zlib"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func deflate(s string) []byte {
	var z bytes.Buffer
	w := zlib.NewWriter(&z)
	w.Write([]byte(s))
	w.Close()
	return z.Bytes()
}

// pdfBuilder writes numbered objects and a trailer.
type pdfBuilder struct{ b bytes.Buffer }

func newPDF() *pdfBuilder {
	p := &pdfBuilder{}
	p.b.WriteString("%PDF-1.7\n")
	return p
}

func (p *pdfBuilder) obj(n int, body string) *pdfBuilder {
	fmt.Fprintf(&p.b, "%d 0 obj\n%s\nendobj\n", n, body)
	return p
}

// stream writes a stream object; dict holds entries besides /Length.
func (p *pdfBuilder) stream(n int, dict string, data []byte) *pdfBuilder {
	fmt.Fprintf(&p.b, "%d 0 obj\n<< %s /Length %d >>\nstream\n", n, dict, len(data))
	p.b.Write(data)
	p.b.WriteString("\nendstream\nendobj\n")
	return p
}

func (p *pdfBuilder) done(root int, extra string) []byte {
	fmt.Fprintf(&p.b, "trailer\n<< /Root %d 0 R %s >>\n%%%%EOF\n", root, extra)
	return p.b.Bytes()
}

// simpleTextPDF has two pages drawn in a WinAnsi Type1 font whose
// /Differences put é at code 1, with compressed content streams.
func simpleTextPDF() []byte {
	page1 := `BT /F1 12 Tf 72 720 Td (Invoice 4471 for Caf\001) Tj 0 -14 Td [(Total)-600(due:)] TJ ( $12.50) Tj ET`
	page2 := `BT /F1 12 Tf 1 0 0 1 72 720 Tm (Second) Tj 1 0 0 1 72 700 Tm (page) Tj T* (\(done\)) Tj ET`
	return newPDF().
		obj(1, "<< /Type /Catalog /Pages 2 0 R >>").
		obj(2, "<< /Type /Pages /Kids [3 0 R 4 0 R] /Count 2 /Resources << /Font << /F1 5 0 R >> >> >>").
		obj(3, "<< /Type /Page /Parent 2 0 R /Contents 6 0 R >>").
		obj(4, "<< /Type /Page /Parent 2 0 R /Contents [7 0 R] >>").
		obj(5, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding << /Type /Encoding /BaseEncoding /WinAnsiEncoding /Differences [1 /eacute] >> >>").
		stream(6, "/Filter /FlateDecode", deflate(page1)).
		stream(7, "/Filter /FlateDecode", deflate(page2)).
		done(1, "")
}

// objStmTextPDF keeps its page tree, page and font in a compressed object
// stream, and draws with a two-byte Type0 font through a ToUnicode map
// (bfchar, a bfrange with a start, and a bfrange with an array).
func objStmTextPDF() []byte {
	cmap := `/CIDInit /ProcSet findresource begin 12 dict begin begincmap
1 begincodespacerange <0000> <FFFF> endcodespacerange
2 beginbfchar <0001> <0048> <0002> <00E9> endbfchar
2 beginbfrange <0010> <0012> <0061> <0020> <0021> [<D83DDE00> <0021>] endbfrange
endcmap CMapName currentdict /CMap defineresource pop end end`
	// H é a b c 😀 !   (and 0099, unmapped, skipped)
	content := `BT /F2 10 Tf 50 50 Td <0001000200100011001200200021> Tj <0099> Tj ET`
	objs := []string{
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /Resources << /Font << /F2 5 0 R >> >> /Contents 6 0 R >>",
		"<< /Type /Font /Subtype /Type0 /BaseFont /X /Encoding /Identity-H /ToUnicode 7 0 R >>",
	}
	nums := []int{2, 3, 5}
	var header, body strings.Builder
	for i, o := range objs {
		fmt.Fprintf(&header, "%d %d ", nums[i], body.Len())
		body.WriteString(o + "\n")
	}
	stm := header.String() + body.String()
	return newPDF().
		obj(1, "<< /Type /Catalog /Pages 2 0 R >>").
		stream(4, fmt.Sprintf("/Type /ObjStm /N 3 /First %d /Filter /FlateDecode", header.Len()), deflate(stm)).
		stream(6, "/Filter /FlateDecode", deflate(content)).
		stream(7, "/Filter /FlateDecode", deflate(cmap)).
		done(1, "")
}

// scannedPDF has a page that only draws an image.
func scannedPDF() []byte {
	return newPDF().
		obj(1, "<< /Type /Catalog /Pages 2 0 R >>").
		obj(2, "<< /Type /Pages /Kids [3 0 R] /Count 1 >>").
		obj(3, "<< /Type /Page /Parent 2 0 R /Resources << /XObject << /Im1 5 0 R >> >> /Contents 4 0 R >>").
		stream(4, "", []byte("q 612 0 0 792 0 0 cm /Im1 Do Q BI /W 2 /H 1 /BPC 8 /CS /G ID \x00(Tj) EI Q")).
		stream(5, "/Type /XObject /Subtype /Image /Width 1 /Height 1 /BitsPerComponent 8 /ColorSpace /DeviceGray", []byte{0}).
		done(1, "")
}

func TestPDFText(t *testing.T) {
	cases := []struct {
		name string
		pdf  []byte
		want string
	}{
		{"simple font, compressed content", simpleTextPDF(), "Invoice 4471 for Café\nTotal due: $12.50\n\nSecond\npage\n(done)"},
		{"object stream, ToUnicode map", objStmTextPDF(), "Héabc😀!"},
		{"scanned", scannedPDF(), ""},
		{"ascii85 content", newPDF().
			obj(1, "<< /Type /Catalog /Pages 2 0 R >>").
			obj(2, "<< /Type /Pages /Kids [3 0 R] /Count 1 >>").
			obj(3, "<< /Type /Page /Parent 2 0 R /Contents 4 0 R >>").
			stream(4, "/Filter [/ASCII85Decode /FlateDecode]", []byte(encodeA85(deflate("BT (Hi there) Tj ET")))).
			done(1, ""), "Hi there"},
		{"no page tree: pages in object order", newPDF().
			obj(3, "<< /Type /Page /Contents 4 0 R >>").
			stream(4, "", []byte("BT (Orphan) Tj ET")).
			done(99, ""), "Orphan"},
	}
	for _, c := range cases {
		got, err := pdfText(c.pdf, 10000)
		if err != nil || got != c.want {
			t.Errorf("%s: got %q, %v; want %q", c.name, got, err, c.want)
		}
	}
}

func encodeA85(b []byte) string {
	var out bytes.Buffer
	enc := make([]byte, 5)
	for i := 0; i < len(b); i += 4 {
		chunk := make([]byte, 4)
		n := copy(chunk, b[i:])
		v := uint32(chunk[0])<<24 | uint32(chunk[1])<<16 | uint32(chunk[2])<<8 | uint32(chunk[3])
		if v == 0 && n == 4 {
			out.WriteByte('z')
			continue
		}
		for j := 4; j >= 0; j-- {
			enc[j] = byte(v%85) + '!'
			v /= 85
		}
		out.Write(enc[:n+1])
		if i%40 == 0 {
			out.WriteByte('\n')
		}
	}
	out.WriteString("~>")
	return out.String()
}

func TestPDFTextLimits(t *testing.T) {
	got, err := pdfText(simpleTextPDF(), 10)
	if err != nil || got != "Invoice 44" {
		t.Errorf("cut at 10: got %q, %v", got, err)
	}
	enc := newPDF().obj(1, "<< /Type /Catalog /Pages 2 0 R >>").done(1, "/Encrypt 9 0 R /ID [<00> <00>]")
	if _, err := pdfText(enc, 100); !errors.Is(err, errPDFEncrypted) {
		t.Errorf("encrypted: got %v", err)
	}
	// A page tree that contains itself and a form that draws itself end.
	loop := newPDF().
		obj(1, "<< /Type /Catalog /Pages 2 0 R >>").
		obj(2, "<< /Type /Pages /Kids [2 0 R 3 0 R] /Count 1 >>").
		obj(3, "<< /Type /Page /Resources << /XObject << /X 4 0 R >> >> /Contents 5 0 R >>").
		stream(4, "/Type /XObject /Subtype /Form", []byte("BT (loop) Tj ET /X Do")).
		stream(5, "", []byte("/X Do")).
		done(1, "")
	done := make(chan string)
	go func() { s, _ := pdfText(loop, 10000); done <- s }()
	select {
	case s := <-done:
		if want := strings.Repeat("loop", maxFormDepth); s != want {
			t.Errorf("self-drawing form: got %q", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a self-referencing PDF didn't finish")
	}
	// 64 MB of zeros as content: the inflate budget stops it.
	var z bytes.Buffer
	w := zlib.NewWriter(&z)
	w.Write(make([]byte, maxTextInflate+1))
	w.Close()
	bomb := newPDF().
		obj(1, "<< /Type /Catalog /Pages 2 0 R >>").
		obj(2, "<< /Type /Pages /Kids [3 0 R] /Count 1 >>").
		obj(3, "<< /Type /Page /Contents 4 0 R >>").
		stream(4, "/Filter /FlateDecode", z.Bytes()).
		done(1, "")
	if s, _ := pdfText(bomb, 100); s != "" {
		t.Errorf("bomb: got %q", s)
	}
}

func FuzzPDFText(f *testing.F) {
	for _, b := range [][]byte{simpleTextPDF(), objStmTextPDF(), scannedPDF(), plainPDF(2), compressedPDF(2, 2)} {
		f.Add(b)
		f.Add(b[:len(b)/2])
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		s, _ := pdfText(b, 2000)
		if len(s) > 2000 {
			t.Fatalf("over the cap: %d", len(s))
		}
	})
}
