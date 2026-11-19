package agent

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"strings"
	"testing"
)

// plainPDF builds an uncompressed PDF with n page objects.
func plainPDF(n int) []byte {
	var b strings.Builder
	b.WriteString("%PDF-1.4\n1 0 obj\n<< /Type /Catalog /Pages 2 0 R /PageLabels << /Nums [0 << /S /D >>] >> >>\nendobj\n")
	fmt.Fprintf(&b, "2 0 obj\n<< /Type /Pages /Kids [] /Count %d >>\nendobj\n", n)
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "%d 0 obj\n<</Type/Page/Parent 2 0 R/Resources<</Font<</F1 5 0 R>>>>>>\nendobj\n", i+10)
	}
	b.WriteString("trailer\n<< /Root 1 0 R >>\n%%EOF\n")
	return []byte(b.String())
}

// compressedPDF builds a PDF whose page tree and pages live only in a
// Flate-compressed object stream, as modern tools write them.
func compressedPDF(pages, count int) []byte {
	var objs strings.Builder
	fmt.Fprintf(&objs, "<< /Type /Pages /Kids [3 0 R] /Count %d /Resources << /Count 9999 >> >>\n", count)
	for i := 0; i < pages; i++ {
		objs.WriteString("<< /Type /Page /Parent 2 0 R >>\n")
	}
	objs.WriteString("<< /Type /Outlines /Count 7000 >>\n")
	var z bytes.Buffer
	w := zlib.NewWriter(&z)
	w.Write([]byte(objs.String()))
	w.Close()
	var b bytes.Buffer
	b.WriteString("%PDF-1.7\n1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")
	fmt.Fprintf(&b, "4 0 obj\n<< /Type /ObjStm /N %d /First 0 /Filter /FlateDecode /Length %d >>\nstream\r\n", pages+2, z.Len())
	b.Write(z.Bytes())
	b.WriteString("\nendstream\nendobj\n%%EOF\n")
	return b.Bytes()
}

func TestPDFPages(t *testing.T) {
	cases := []struct {
		name string
		pdf  []byte
		want int
	}{
		{"plain", plainPDF(3), 3},
		{"plain, /Count larger than objects found", bytes.Replace(plainPDF(2), []byte("/Count 2"), []byte("/Count 80"), 1), 80},
		{"compressed, counted by /Count", compressedPDF(1, 300), 300},
		{"compressed, counted by objects", compressedPDF(60, 0), 60},
		{"not a pdf at all", []byte("hello"), 0},
	}
	for _, c := range cases {
		if got := pdfPages(c.pdf); got != c.want {
			t.Errorf("%s: got %d pages, want %d", c.name, got, c.want)
		}
	}
}

func TestPDFPagesInflateIsBounded(t *testing.T) {
	// 64 MB of zeros compresses to a few tens of KB; counting must stop at
	// the inflate budget instead of expanding all of it.
	var z bytes.Buffer
	w := zlib.NewWriter(&z)
	w.Write(make([]byte, 64<<20))
	w.Close()
	pdf := append([]byte("%PDF-1.7\n4 0 obj\n<< /Type /ObjStm /Filter /FlateDecode >>\nstream\n"), z.Bytes()...)
	pdf = append(pdf, []byte("\nendstream\nendobj\n")...)
	budget := maxObjStmBytes
	got := objectStreams(pdf, &budget)
	if len(got) != 1 || len(got[0]) != maxObjStmBytes || budget != 0 {
		t.Fatalf("inflated %d streams, budget left %d", len(got), budget)
	}
}

func TestPDFPagesSkipsOtherFilters(t *testing.T) {
	pdf := []byte("%PDF-1.7\n4 0 obj\n<< /Type /ObjStm /Filter /LZWDecode >>\nstream\nxxxx\nendstream\nendobj\n")
	budget := maxObjStmBytes
	if got := objectStreams(pdf, &budget); len(got) != 0 {
		t.Fatalf("read %d streams with a filter it doesn't support", len(got))
	}
}
