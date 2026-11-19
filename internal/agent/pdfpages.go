package agent

import (
	"bytes"
	"compress/zlib"
	"errors"
	"io"
	"strconv"
)

const (
	// maxObjStmBytes bounds how much Thicket inflates while counting
	// pages, in total across a file's object streams, so a small PDF can't
	// expand into gigabytes.
	maxObjStmBytes = 32 << 20
	// maxDictScan bounds how many bytes the dictionary searches may look
	// at across one file, so a file full of /Type tokens can't make
	// counting quadratic.
	maxDictScan = 64 << 20
	// maxDictReach is how far one search goes for a dictionary's ends.
	maxDictReach = 64 << 10
)

// pdfScan is the state of counting one file.
type pdfScan struct {
	work     int  // bytes the dictionary searches may still look at
	complete bool // false once something couldn't be read
}

// pdfPages estimates a PDF's page count. It reads both the page objects
// and the page tree's /Count, in the file itself and in its compressed
// object streams (where modern tools keep them), and returns the larger:
// an estimate that errs towards more pages. complete is false when part
// of the file couldn't be read (an object stream cut off by the inflate
// budget, damaged or in another compression, or the search budget ran
// out): pages is then a lower bound.
func pdfPages(b []byte) (pages int, complete bool) {
	sc := &pdfScan{work: maxDictScan, complete: true}
	pages, count := sc.pagesIn(b)
	for _, s := range sc.objectStreams(b) {
		p, c := sc.pagesIn(s)
		pages += p
		count = max(count, c)
	}
	return max(pages, count), sc.complete
}

// pagesIn reads one buffer of uncompressed PDF objects: the number of
// /Type /Page objects, and the largest /Count of a /Type /Pages node (the
// root of the page tree counts every page).
func (sc *pdfScan) pagesIn(b []byte) (pages, count int) {
	for i := 0; ; {
		j := bytes.Index(b[i:], []byte("/Type"))
		if j < 0 {
			break
		}
		at := i + j
		i = at + len("/Type")
		rest := bytes.TrimLeft(b[i:min(len(b), i+16)], " \t\r\n\f\x00")
		switch {
		case bytes.HasPrefix(rest, []byte("/Pages")) && !nameContinues(rest, len("/Pages")):
			if lo, hi, ok := sc.dictAround(b, at); ok {
				count = max(count, dictCount(b[lo:hi]))
			}
		case bytes.HasPrefix(rest, []byte("/Page")) && !nameContinues(rest, len("/Page")):
			pages++
		}
	}
	return pages, count
}

// nameContinues reports whether the PDF name starting rest goes on past
// n bytes (so /PageLabels isn't /Page).
func nameContinues(rest []byte, n int) bool {
	if len(rest) <= n {
		return false
	}
	switch rest[n] {
	case ' ', '\t', '\r', '\n', '\f', '\x00', '/', '<', '>', '[', ']', '(', ')', '{', '}', '%':
		return false
	}
	return true
}

// dictAround finds the dictionary that contains position at, nesting
// aware: b[lo:hi] runs from its << to its >>. ok is false when it can't
// be found within reach, or when the file's search budget is spent (which
// marks the scan incomplete).
func (sc *pdfScan) dictAround(b []byte, at int) (lo, hi int, ok bool) {
	if sc.work <= 0 {
		sc.complete = false
		return 0, 0, false
	}
	// Back to the << that opens the enclosing dictionary.
	depth, start := 0, -1
	i := at - 1
	for ; i > 0 && start < 0 && at-i < maxDictReach; i-- {
		switch {
		case b[i] == '>' && b[i-1] == '>':
			depth++
			i--
		case b[i] == '<' && b[i-1] == '<':
			if depth == 0 {
				start = i - 1
			} else {
				depth--
			}
			i--
		}
	}
	sc.work -= at - i
	if start < 0 {
		return 0, 0, false
	}
	// Forward to the matching >>.
	depth = 0
	j := start
	for ; j+1 < len(b) && j-start < 2*maxDictReach; j++ {
		switch {
		case b[j] == '<' && b[j+1] == '<':
			depth++
			j++
		case b[j] == '>' && b[j+1] == '>':
			depth--
			j++
			if depth == 0 {
				sc.work -= j - start
				return start, j + 1, true
			}
		}
	}
	sc.work -= j - start
	return 0, 0, false
}

// dictCount returns the /Count entry of dictionary d (from << to >>), or
// 0. Only its own entries are read, not those of dictionaries nested in
// it.
func dictCount(d []byte) int {
	depth := 0
	for i := 0; i+1 < len(d); i++ {
		switch {
		case d[i] == '<' && d[i+1] == '<':
			depth++
			i++
		case d[i] == '>' && d[i+1] == '>':
			depth--
			i++
		case depth == 1 && bytes.HasPrefix(d[i:], []byte("/Count")) && !nameContinues(d[i:], len("/Count")):
			j := i + len("/Count")
			for j < len(d) && (d[j] == ' ' || d[j] == '\t' || d[j] == '\r' || d[j] == '\n') {
				j++
			}
			k := j
			for k < len(d) && d[k] >= '0' && d[k] <= '9' && k-j < 9 {
				k++
			}
			n, err := strconv.Atoi(string(d[j:k]))
			if err != nil {
				return 0
			}
			return n
		}
	}
	return 0
}

// objectStreams inflates the file's Flate-compressed object streams
// (/Type /ObjStm) within the inflate budget. A stream it can't read in
// full (another filter, damaged, or cut off by the budget) marks the scan
// incomplete; what was read of it is still counted.
func (sc *pdfScan) objectStreams(b []byte) [][]byte {
	var out [][]byte
	budget := maxObjStmBytes
	for i := 0; ; {
		j := bytes.Index(b[i:], []byte("/ObjStm"))
		if j < 0 {
			return out
		}
		at := i + j
		i = at + len("/ObjStm")
		lo, hi, ok := sc.dictAround(b, at)
		if !ok {
			sc.complete = false
			continue
		}
		// The stream's data follows its dictionary and the stream keyword.
		rest := bytes.TrimLeft(b[hi:], " \t\r\n\f\x00")
		if !bytes.HasPrefix(rest, []byte("stream")) {
			continue // a mention of /ObjStm that isn't a stream's type
		}
		if !bytes.Contains(b[lo:hi], []byte("/FlateDecode")) {
			sc.complete = false
			continue
		}
		data := len(b) - len(rest) + len("stream")
		if data < len(b) && b[data] == '\r' {
			data++
		}
		if data < len(b) && b[data] == '\n' {
			data++
		}
		i = data
		if budget <= 0 {
			sc.complete = false
			continue
		}
		r, err := zlib.NewReader(bytes.NewReader(b[data:]))
		if err != nil {
			sc.complete = false
			continue
		}
		// One byte over the budget tells a cut-off stream from one that
		// fits exactly.
		s, err := io.ReadAll(io.LimitReader(r, int64(budget)+1))
		r.Close()
		if len(s) > budget {
			s = s[:budget]
			sc.complete = false
		}
		if err != nil && !errors.Is(err, io.EOF) {
			sc.complete = false
		}
		budget -= len(s)
		if len(s) > 0 {
			out = append(out, s)
		}
	}
}
