package websearch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Result is one search result.
type Result struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"content"`
}

// SearXNG queries a SearXNG instance's JSON API. The instance is set by
// the owner (often on the local network), so it isn't held to the
// public-address rule pages are.
func SearXNG(ctx context.Context, base, query string, n int) ([]Result, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	u := strings.TrimRight(base, "/") + "/search?" + url.Values{"q": {query}, "format": {"json"}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("couldn't reach the search server: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("the search server refused JSON (HTTP 403): add json to search.formats in SearXNG's settings.yml")
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("the search server answered HTTP %d", resp.StatusCode)
	}
	var out struct {
		Results []Result `json:"results"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("unreadable search results: %w", err)
	}
	if len(out.Results) > n {
		out.Results = out.Results[:n]
	}
	return out.Results, nil
}

// Format lists results for the model.
func Format(results []Result) string {
	if len(results) == 0 {
		return "No results."
	}
	var b strings.Builder
	for i, r := range results {
		fmt.Fprintf(&b, "[%d] %s\n%s\n", i+1, strings.TrimSpace(r.Title), r.URL)
		if s := strings.TrimSpace(r.Snippet); s != "" {
			if len(s) > 400 {
				s = s[:400] + "…"
			}
			b.WriteString(s + "\n")
		}
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}
