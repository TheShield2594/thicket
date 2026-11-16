package agent

import (
	"context"
	"encoding/base64"
	"fmt"
	"mime"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/theshield2594/thicket/internal/channel"
	"github.com/theshield2594/thicket/internal/llm"
)

// Attachment limits. Images go to the model for this turn only; text
// files are read into the message (and so into the history).
const (
	maxImagesPerMessage = 4
	maxImageBytes       = 5 << 20
	maxTextFileBytes    = 256 << 10
	maxTextFileChars    = 12000
	maxFilesPerMessage  = 8
)

var imageTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

var textExts = map[string]bool{
	".txt": true, ".md": true, ".markdown": true, ".csv": true, ".tsv": true, ".json": true, ".yaml": true, ".yml": true,
	".toml": true, ".xml": true, ".html": true, ".htm": true, ".log": true, ".ini": true, ".conf": true, ".cfg": true,
	".go": true, ".py": true, ".js": true, ".ts": true, ".sh": true, ".sql": true, ".rs": true, ".java": true, ".c": true,
	".h": true, ".cpp": true, ".rb": true, ".php": true, ".css": true, ".ics": true, ".vcf": true, ".srt": true,
}

type attachmentKind int

const (
	kindOther attachmentKind = iota
	kindImage
	kindText
)

func classify(a channel.Attachment) attachmentKind {
	ct := baseType(a.ContentType)
	ext := strings.ToLower(path.Ext(a.Name))
	if ct == "" {
		ct = baseType(mime.TypeByExtension(ext))
	}
	switch {
	case imageTypes[ct]:
		return kindImage
	case strings.HasPrefix(ct, "text/"), ct == "application/json", ct == "application/xml", textExts[ext]:
		return kindText
	}
	return kindOther
}

func baseType(ct string) string {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.ToLower(strings.TrimSpace(ct))
}

// attachmentNotes is the short marker text for each attachment, appended
// to the message before anything is downloaded. Approval and secret
// handling see these, as they always have.
func attachmentNotes(atts []channel.Attachment) string {
	var b strings.Builder
	for _, a := range atts {
		label := "attachment you can't open"
		switch classify(a) {
		case kindImage:
			label = "image"
		case kindText:
			label = "file"
		}
		fmt.Fprintf(&b, "\n[%s: %s]", label, cleanName(a.Name))
	}
	return b.String()
}

// cleanName keeps a file name from breaking out of its marker.
func cleanName(n string) string {
	n = strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '[', ']', '<', '>', '"':
			return ' '
		}
		return r
	}, n)
	if n = strings.TrimSpace(n); n == "" {
		n = "unnamed"
	}
	return truncate(n, 100)
}

// loadAttachments downloads what the model can use: images become vision
// input for this turn, text files are read into the message text (secrets
// redacted, wrapped as untrusted data). It returns the text to append and
// the images. Failures become notes, never errors: a bad file must not
// cost the user their reply.
func (e *Engine) loadAttachments(ctx context.Context, atts []channel.Attachment) (string, []llm.Image) {
	var b strings.Builder
	var images []llm.Image
	files := 0
	for _, a := range atts {
		name := cleanName(a.Name)
		switch classify(a) {
		case kindImage:
			if len(images) >= maxImagesPerMessage {
				fmt.Fprintf(&b, "\n[image %s was not shown to you: only %d images per message]", name, maxImagesPerMessage)
				continue
			}
			body, ct, err := e.fetch(ctx, a, maxImageBytes)
			if err == nil && !imageTypes[baseType(ct)] {
				err = fmt.Errorf("not a supported image type")
			}
			if err != nil {
				fmt.Fprintf(&b, "\n[image %s could not be loaded: %v]", name, err)
				continue
			}
			images = append(images, llm.Image{URL: "data:" + baseType(ct) + ";base64," + base64.StdEncoding.EncodeToString(body)})
		case kindText:
			if files >= maxFilesPerMessage {
				fmt.Fprintf(&b, "\n[file %s was not read: only %d files per message]", name, maxFilesPerMessage)
				continue
			}
			files++
			body, _, err := e.fetch(ctx, a, maxTextFileBytes)
			if err == nil && !utf8.Valid(body) {
				err = fmt.Errorf("not UTF-8 text")
			}
			if err != nil {
				fmt.Fprintf(&b, "\n[file %s could not be read: %v]", name, err)
				continue
			}
			content := e.Secrets.Redact(string(body))
			if len(content) > maxTextFileChars {
				cut := maxTextFileChars
				for cut > 0 && !utf8.RuneStart(content[cut]) {
					cut--
				}
				content = content[:cut] + "\n[file truncated]"
			}
			b.WriteString("\n" + wrapUntrusted("file "+name, content))
		}
	}
	return b.String(), images
}

func (e *Engine) fetch(ctx context.Context, a channel.Attachment, max int) ([]byte, string, error) {
	if e.FetchAttachment != nil {
		return e.FetchAttachment(ctx, a, max)
	}
	return channel.Fetch(ctx, a, max)
}

// withoutImages replaces images with a note, for models that refuse them.
func withoutImages(msgs []llm.Message) []llm.Message {
	out := make([]llm.Message, len(msgs))
	for i, m := range msgs {
		if len(m.Images) > 0 {
			m.Content += fmt.Sprintf("\n[%d image(s) attached, but the model can't view images. Tell the user you can't see them.]", len(m.Images))
			m.Images = nil
		}
		out[i] = m
	}
	return out
}
