package agent

import (
	"bytes"
	"compress/zlib"
	"io"
	"strconv"
)

// maxObjStmBytes bounds how much Thicket inflates while counting pages,
// in total across a file's object streams, so a small PDF can't expand
// into gigabytes.
const maxObjStmBytes = 32 << 20

// pdfPages estimates a PDF's page count, or 0 if it can't tell. It reads
// both the page objects and the page tree's /Count, in the file itself
// and in its compressed object streams (where modern tools keep them),
// and returns the larger: an estimate that errs towards more pages.
func pdfPages(b []byte) int {
	pages, count := pdfPagesIn(b)
	budget := maxObjStmBytes
	for _, s := range objectStreams(b, &budget) {
		p, c := pdfPagesIn(s)
		pages += p
		count = max(count, c)
	}
	return max(pages, count)
}

// pdfPagesIn reads one buffer of uncompressed PDF objects: the number of
// /Type /Page objects, and the largest /Count of a /Type /Pages node (the
// root of the page tree counts every page).
func pdfPagesIn(b []byte) (pages, count int) {
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
			count = max(count, dictCount(b, at))
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

// dictCount returns the /Count entry of the dictionary that contains
// position at, or 0. Only the dictionary's own entries are read, not
// those of dictionaries nested in it.
func dictCount(b []byte, at int) int {
	// Back to the << that opens the enclosing dictionary.
	depth, start := 0, -1
	for i := at - 1; i > 0 && start < 0 && at-i < 64<<10; i-- {
		switch {
		case b[i] == '>' && b[i-1] == '>':
			depth++
			i--
		case b[i] == '<' && b[i-1] == '<':
			if depth == 0 {
				start = i + 1
			} else {
				depth--
			}
			i--
		}
	}
	if start < 0 {
		return 0
	}
	// Forward through its entries at depth 1, up to the matching >>.
	depth = 0
	for i := start - 2; i+1 < len(b) && i-start < 64<<10; i++ {
		switch {
		case b[i] == '<' && b[i+1] == '<':
			depth++
			i++
		case b[i] == '>' && b[i+1] == '>':
			depth--
			i++
			if depth == 0 {
				return 0
			}
		case depth == 1 && bytes.HasPrefix(b[i:], []byte("/Count")) && !nameContinues(b[i:], len("/Count")):
			j := i + len("/Count")
			for j < len(b) && (b[j] == ' ' || b[j] == '\t' || b[j] == '\r' || b[j] == '\n') {
				j++
			}
			k := j
			for k < len(b) && b[k] >= '0' && b[k] <= '9' && k-j < 9 {
				k++
			}
			n, err := strconv.Atoi(string(b[j:k]))
			if err != nil {
				return 0
			}
			return n
		}
	}
	return 0
}

// objectStreams inflates the file's Flate-compressed object streams
// (/Type /ObjStm), spending at most *budget bytes of output in total.
// Streams it can't read are skipped.
func objectStreams(b []byte, budget *int) [][]byte {
	var out [][]byte
	for i := 0; *budget > 0; {
		j := bytes.Index(b[i:], []byte("/ObjStm"))
		if j < 0 {
			return out
		}
		at := i + j
		i = at + len("/ObjStm")
		// The stream's dictionary ends before the stream keyword; it must
		// ask for Flate, the only filter this reads.
		k := bytes.Index(b[i:], []byte("stream"))
		if k < 0 {
			return out
		}
		dictEnd := i + k
		if bytes.Contains(b[i:dictEnd], []byte("endstream")) {
			continue
		}
		lo := bytes.LastIndex(b[max(0, at-4096):at], []byte("<<"))
		if lo < 0 || !bytes.Contains(b[max(0, at-4096)+lo:dictEnd], []byte("/FlateDecode")) {
			i = dictEnd
			continue
		}
		data := dictEnd + len("stream")
		if data < len(b) && b[data] == '\r' {
			data++
		}
		if data < len(b) && b[data] == '\n' {
			data++
		}
		r, err := zlib.NewReader(bytes.NewReader(b[data:]))
		if err != nil {
			i = data
			continue
		}
		s, _ := io.ReadAll(io.LimitReader(r, int64(*budget)))
		r.Close()
		*budget -= len(s)
		if len(s) > 0 {
			out = append(out, s)
		}
		i = data
	}
	return out
}
