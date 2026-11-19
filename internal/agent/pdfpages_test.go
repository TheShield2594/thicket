package agent

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"strings"
	"testing"
	"time"
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
	// /Filter comes first and a nested /DecodeParms dictionary sits between
	// it and /Type /ObjStm: the nearest << is DecodeParms', so the filter is
	// found only by finding the enclosing dictionary.
	parms := bytes.Replace(compressedPDF(1, 300), []byte("<< /Type /ObjStm /N 3 /First 0 /Filter /FlateDecode"),
		[]byte("<< /Filter /FlateDecode /DecodeParms << /Predictor 1 >> /Type /ObjStm /N 3 /First 0"), 1)
	if !bytes.Contains(parms, []byte("/DecodeParms")) {
		t.Fatal("fixture didn't change")
	}
	cases := []struct {
		name     string
		pdf      []byte
		want     int
		complete bool
	}{
		{"plain", plainPDF(3), 3, true},
		{"plain, /Count larger than objects found", bytes.Replace(plainPDF(2), []byte("/Count 2"), []byte("/Count 80"), 1), 80, true},
		{"compressed, counted by /Count", compressedPDF(1, 300), 300, true},
		{"compressed, counted by objects", compressedPDF(60, 0), 60, true},
		{"compressed, nested DecodeParms", parms, 300, true},
		{"compressed, other filter", bytes.Replace(compressedPDF(1, 300), []byte("/FlateDecode"), []byte("/LZWDecode"), 1), 0, false},
		{"compressed, cut off", compressedPDF(1, 300)[:200], 0, false},
		{"not a pdf at all", []byte("hello"), 0, true},
	}
	for _, c := range cases {
		got, complete := pdfPages(c.pdf)
		if got != c.want || complete != c.complete {
			t.Errorf("%s: got %d pages (complete %v), want %d (complete %v)", c.name, got, complete, c.want, c.complete)
		}
	}
}

func TestPDFPagesInflateIsBounded(t *testing.T) {
	// 64 MB of zeros compresses to a few tens of KB; counting must stop at
	// the inflate budget instead of expanding all of it, and say the count
	// is incomplete.
	var z bytes.Buffer
	w := zlib.NewWriter(&z)
	w.Write(make([]byte, 64<<20))
	w.Close()
	pdf := append([]byte("%PDF-1.7\n4 0 obj\n<< /Type /ObjStm /Filter /FlateDecode >>\nstream\n"), z.Bytes()...)
	pdf = append(pdf, []byte("\nendstream\nendobj\n")...)
	sc := &pdfScan{work: maxDictScan, complete: true}
	got := sc.objectStreams(pdf)
	if len(got) != 1 || len(got[0]) != maxObjStmBytes || sc.complete {
		t.Fatalf("inflated %d streams, complete %v", len(got), sc.complete)
	}
}

func TestPDFPagesSearchIsBounded(t *testing.T) {
	// Many /Type /Pages tokens with no dictionary around them: each search
	// gives up within reach, and the file-wide budget stops the rest.
	pdf := append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte("/Type /Pages "), 8<<20/13)...)
	start := time.Now()
	_, complete := pdfPages(pdf)
	if complete {
		t.Error("an exhausted search budget should make the count incomplete")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("counting took %v", d)
	}
}
