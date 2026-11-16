package channel

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// fetchClient downloads attachments. Redirects stay on https.
var fetchClient = &http.Client{
	Timeout: 30 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" {
			return fmt.Errorf("refusing non-https redirect")
		}
		if len(via) >= 3 {
			return fmt.Errorf("too many redirects")
		}
		return nil
	},
}

// Fetch downloads an attachment, refusing anything over max bytes. It
// returns the body and the content type (from the response, else the
// platform's report, else the file extension).
func Fetch(ctx context.Context, a Attachment, max int) ([]byte, string, error) {
	if a.Size > max {
		return nil, "", fmt.Errorf("too large (%s, limit %s)", humanSize(a.Size), humanSize(max))
	}
	if a.Open != nil {
		body, ct, err := a.Open(ctx, max)
		if err != nil {
			return nil, "", err
		}
		if ct == "" || strings.HasPrefix(ct, "application/octet-stream") {
			ct = a.ContentType
		}
		if ct == "" {
			ct = mime.TypeByExtension(strings.ToLower(path.Ext(a.Name)))
		}
		return body, ct, nil
	}
	u, err := url.Parse(a.URL)
	if err != nil || u.Scheme != "https" {
		return nil, "", fmt.Errorf("unsupported attachment URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := fetchClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, "", fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(max)+1))
	if err != nil {
		return nil, "", err
	}
	if len(body) > max {
		return nil, "", fmt.Errorf("too large (limit %s)", humanSize(max))
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" || strings.HasPrefix(ct, "application/octet-stream") {
		ct = a.ContentType
	}
	if ct == "" {
		ct = mime.TypeByExtension(strings.ToLower(path.Ext(a.Name)))
	}
	return body, ct, nil
}

// HumanSize formats a byte count for messages.
func HumanSize(n int) string { return humanSize(n) }

func humanSize(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}
