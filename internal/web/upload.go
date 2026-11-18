package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/theshield2594/thicket/internal/channel"
)

// Files sent from the web app come with the message as a multipart form.
// They are held in memory only until the reply has read them, as with
// Discord and Telegram, where the file stays on the platform: Thicket
// never stores the bytes. The engine applies the per-kind limits (images,
// PDFs, voice messages); these only bound one request.
const (
	maxUploadFiles = 8
	maxUploadBytes = 30 << 20
)

// readPost reads a message: JSON {"text": ...}, or a multipart form with
// a "text" field and "file" parts.
func readPost(w http.ResponseWriter, r *http.Request) (string, []channel.Attachment, error) {
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct != "multipart/form-data" {
		var body struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
			return "", nil, errors.New("bad request")
		}
		return strings.TrimSpace(body.Text), nil, nil
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	mr, err := r.MultipartReader()
	if err != nil {
		return "", nil, errors.New("bad request")
	}
	var text string
	var atts []channel.Attachment
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				return "", nil, fmt.Errorf("files can be %s in all at most", channel.HumanSize(maxUploadBytes))
			}
			return "", nil, errors.New("bad request")
		}
		switch p.FormName() {
		case "text":
			b, err := io.ReadAll(io.LimitReader(p, maxTextLen*4+1))
			if err != nil {
				return "", nil, errors.New("bad request")
			}
			text = strings.TrimSpace(string(b))
		case "file":
			if len(atts) >= maxUploadFiles {
				return "", nil, fmt.Errorf("at most %d files per message", maxUploadFiles)
			}
			body, err := io.ReadAll(p)
			if err != nil {
				var mbe *http.MaxBytesError
				if errors.As(err, &mbe) {
					return "", nil, fmt.Errorf("files can be %s in all at most", channel.HumanSize(maxUploadBytes))
				}
				return "", nil, errors.New("bad request")
			}
			name := path.Base(strings.ReplaceAll(p.FileName(), "\\", "/"))
			if name == "." || name == "/" || name == "" {
				name = "file"
			}
			atts = append(atts, memAttachment(name, p.Header.Get("Content-Type"), body))
		}
		p.Close()
	}
	return text, atts, nil
}

// memAttachment serves a file already in memory to the engine.
func memAttachment(name, ct string, body []byte) channel.Attachment {
	if base, _, err := mime.ParseMediaType(ct); err == nil {
		ct = base
	} else {
		ct = ""
	}
	return channel.Attachment{Name: name, ContentType: ct, Size: len(body),
		Open: func(_ context.Context, max int) ([]byte, string, error) {
			if len(body) > max {
				return nil, "", fmt.Errorf("too large (%s, limit %s)", channel.HumanSize(len(body)), channel.HumanSize(max))
			}
			return body, ct, nil
		}}
}

// withFileNames is what the chat shows for a message: its text, then a
// line per file.
func withFileNames(text string, atts []channel.Attachment) string {
	lines := []string{}
	if text != "" {
		lines = append(lines, text)
	}
	for _, a := range atts {
		mark := "📎"
		if strings.HasPrefix(a.ContentType, "audio/") {
			mark = "🎤"
		}
		lines = append(lines, mark+" "+a.Name)
	}
	return strings.Join(lines, "\n")
}
