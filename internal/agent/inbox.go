package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/theshield2594/thicket/internal/store"
)

// Inbox lists what needs the user: open inbox items and approvals still
// waiting. userID 0 means everyone's.
func (e *Engine) Inbox(ctx context.Context, userID int64) ([]store.InboxItem, []store.Approval, error) {
	items, err := e.DB.OpenInbox(ctx, userID)
	if err != nil {
		return nil, nil, err
	}
	apps, err := e.DB.PendingApprovals(ctx, userID, approvalMaxAge)
	return items, apps, err
}

func (e *Engine) toolNeedsYou(ctx context.Context, t *turn, args json.RawMessage) (string, error) {
	a, err := decode[struct {
		Action string `json:"action"`
		ID     int64  `json:"id"`
	}](args)
	if err != nil {
		return "", err
	}
	if a.Action == "resolve" {
		if err := e.DB.ResolveInbox(ctx, a.ID, t.user.ID); errors.Is(err, store.ErrNotFound) {
			return "", fmt.Errorf("no open inbox item #%d", a.ID)
		} else if err != nil {
			return "", err
		}
		return fmt.Sprintf("Inbox item #%d resolved.", a.ID), nil
	}
	items, apps, err := e.Inbox(ctx, t.user.ID)
	if err != nil {
		return "", err
	}
	if len(items) == 0 && len(apps) == 0 {
		return "The inbox is empty: nothing needs the user.", nil
	}
	var b strings.Builder
	for _, ap := range apps {
		fmt.Fprintf(&b, "- approval waiting: %s wants to run %s (reply approve or deny in that chat)\n", e.charName(ap.Character), ap.Tool)
	}
	for _, it := range items {
		n := ""
		if it.Count > 1 {
			n = fmt.Sprintf(" (×%d)", it.Count)
		}
		fmt.Fprintf(&b, "- #%d [%s] %s%s, %s: %s\n", it.ID, it.Kind, it.Title, n, it.UpdatedAt.In(t.user.Location()).Format("Mon 2 Jan 15:04"), truncate(strings.ReplaceAll(it.Detail, "\n", " "), 300))
	}
	return b.String(), nil
}
