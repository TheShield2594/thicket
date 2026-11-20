package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/mail"
	"github.com/theshield2594/thicket/internal/mcp"
	"github.com/theshield2594/thicket/internal/store"
)

// A sprig's own email address (docs/mail.md). The thicket-mail plugin
// speaks JMAP or IMAP/SMTP and enforces the sending rules; core keeps
// who may do what:
//
//   - The address and its password: set up by the sprig's owner (in
//     chat; the password is captured from their next DM into the
//     encrypted store and pushed to the plugin, which keeps it in memory).
//     Kid sprigs can't have one.
//   - The allowlist: who the sprig may email without a person's OK. Only
//     the sprig's owner changes it (chat, web, API); every change is in
//     the trail and can be undone. Each call to the plugin carries it as
//     it is at that moment, so a removal counts at once.
//   - Who is calling: every call to one of Thicket's own plugins says
//     which sprig, whether a person approved it, and whether the sprig is
//     at draft level (pluginCtx). The plugin never takes the sprig from
//     the model's arguments.
//   - Drafts: a message the plugin drafted (off the allowlist, over the
//     daily limit, at draft level) lands in "needs you": Do it sends it,
//     dismissing it discards it.
//   - Events: every message sent or received is in the trail (from, to,
//     subject, size, message id; never the body). New mail checks the
//     sprig's mail triggers at once. A one-time code is shown to a
//     person in "needs you"; the sprig sees it hidden.

type callFlag int

const (
	approvedFlag  callFlag = iota + 1 // a person approved this exact call
	draftOnlyFlag                     // the sprig is at draft level: only draft
)

func withFlag(ctx context.Context, f callFlag) context.Context {
	return context.WithValue(ctx, f, true)
}

func hasFlag(ctx context.Context, f callFlag) bool {
	v, _ := ctx.Value(f).(bool)
	return v
}

// managedPlugins lists the integrations that are Thicket's own plugins
// (set up by it, with the token it shares): only they get to know who
// is calling, and only their tool hints are honoured.
func (e *Engine) managedPlugins(ctx context.Context) map[string]bool {
	out := map[string]bool{}
	servers, err := e.DB.MCPServers(ctx)
	if err != nil {
		return out
	}
	for _, s := range servers {
		if p, ok := e.plugins().Get(s.Name); ok && s.Enabled && s.Transport == "http" && s.Headers["Authorization"] == "Bearer {{secret:"+p.Secret()+"}}" {
			out[s.Name] = true
		}
	}
	return out
}

// pluginCtx adds who is calling to a call to one of Thicket's own
// plugins (plugins/mail/PROTOCOL.md): the sprig, whether a person
// approved the call, whether it may only draft, and for mail the
// sprig's allowlist as it is now.
func (e *Engine) pluginCtx(ctx context.Context, sprig, server string) (context.Context, error) {
	meta := map[string]any{"thicket/call": map[string]any{
		"sprig":      sprig,
		"approved":   hasFlag(ctx, approvedFlag),
		"draft_only": hasFlag(ctx, draftOnlyFlag),
	}}
	if server == mail.Plugin {
		allow, err := e.DB.MailAllow(ctx, sprig)
		if err != nil {
			return ctx, fmt.Errorf("couldn't read %s's mail allowlist: %w", sprig, err)
		}
		meta["thicket/mail"] = map[string]any{"allow": allow}
	}
	return mcp.WithMeta(ctx, meta), nil
}

// mailOwner is who a sprig's mail is for: a private sprig's person, else
// whoever set its address up.
func (e *Engine) mailOwner(ctx context.Context, sprig string) int64 {
	if c, ok := e.Chars.Get(sprig); ok && c.BelongsTo != 0 {
		return c.BelongsTo
	}
	if a, err := e.DB.MailAccount(ctx, sprig); err == nil {
		return a.CreatedBy
	}
	return 0
}

// MayReadMail reports whether u may open sprig c's mailbox: a private
// sprig's only for its person (admins included: not theirs), a shared
// sprig's for adults. Kid sprigs have none, and kids see none.
func MayReadMail(u *store.User, c *character.Character) bool {
	switch {
	case u == nil || c == nil || u.IsKid() || c.IsKid():
		return false
	case c.Tier() == character.TierSolo:
		return c.BelongsTo == u.ID
	}
	return true
}

// mailDraftAction is a "needs you" item for a draft the plugin saved.
type mailDraftAction struct {
	Mail struct {
		Sprig   string `json:"sprig"`
		DraftID string `json:"draft_id"`
	} `json:"mail"`
	ChatRef string `json:"chat_ref,omitempty"`
}

// MailDraftOf reads a "needs you" item's mail draft, if it is one.
func MailDraftOf(action string) (sprig, draftID string, ok bool) {
	var a mailDraftAction
	if json.Unmarshal([]byte(action), &a) != nil || a.Mail.DraftID == "" || a.Mail.Sprig == "" {
		return "", "", false
	}
	return a.Mail.Sprig, a.Mail.DraftID, true
}

// mailDrafted puts a draft the plugin saved in "needs you". It returns
// what the sprig should be told if that failed ("" if it worked): the
// draft is saved in the mailbox either way, and isn't made again.
func (e *Engine) mailDrafted(ctx context.Context, t *turn, raw json.RawMessage) string {
	var d mail.Drafted
	if err := json.Unmarshal(raw, &d); err != nil || d.DraftID == "" {
		e.Log.Warn("mail plugin drafted something unreadable", "sprig", t.char.ID)
		return "\n(Note: the draft is in the mailbox's Drafts, but it couldn't be added to needs you. Tell the person to send it from the Mail page on the dashboard.)"
	}
	var act mailDraftAction
	act.Mail.Sprig, act.Mail.DraftID, act.ChatRef = t.char.ID, d.DraftID, t.chatRef
	b, _ := json.Marshal(act)
	var detail strings.Builder
	fmt.Fprintf(&detail, "From: %s\nTo: %s\n", d.From, strings.Join(d.To, ", "))
	if len(d.Cc) > 0 {
		fmt.Fprintf(&detail, "Cc: %s\n", strings.Join(d.Cc, ", "))
	}
	if len(d.Bcc) > 0 {
		fmt.Fprintf(&detail, "Bcc: %s\n", strings.Join(d.Bcc, ", "))
	}
	fmt.Fprintf(&detail, "Subject: %s\n\n%s", d.Subject, d.Preview)
	if d.Reason != "" {
		fmt.Fprintf(&detail, "\n\nNot sent because %s.", strings.TrimSuffix(d.Reason, "."))
	}
	to := "(no one)"
	if len(d.To) > 0 {
		to = d.To[0]
		if len(d.To) > 1 {
			to += fmt.Sprintf(" and %d more", len(d.To)-1)
		}
	}
	_, err := e.DB.AddInbox(ctx, store.InboxItem{UserID: heldOwner(t), Character: t.char.ID, Kind: store.InboxDraft,
		Title:  truncate(fmt.Sprintf("%s drafted an email to %s: %s", t.char.DisplayName(), to, d.Subject), 200),
		Detail: truncate(e.Secrets.Redact(detail.String()), 4000), Action: string(b)})
	if err != nil {
		e.Log.Error("save mail draft to needs you", "err", err)
		return "\n(Note: the draft is in the mailbox's Drafts, but it couldn't be added to needs you. Tell the person to send it from the Mail page on the dashboard.)"
	}
	return ""
}

// mailSendLevel is a sprig's level for sending a mail draft (the plugin's
// send_draft), as it would apply to a call now.
func (e *Engine) mailSendLevel(ctx context.Context, c *character.Character) string {
	return e.toolLevel(ctx, c, &Tool{Name: mcp.FuncName(mail.Plugin, "send_draft"), Scope: "mcp:" + mail.Plugin, Risky: true, GatesItself: true})
}

// sendMailDraft sends a mail draft a person said yes to in "needs you".
func (e *Engine) sendMailDraft(ctx context.Context, user *store.User, it *store.InboxItem, sprig, draftID string) error {
	if e.Mail == nil {
		return errors.New("the mail plugin isn't running")
	}
	c, ok := e.Chars.Get(sprig)
	if !ok {
		return fmt.Errorf("the sprig %q no longer exists", sprig)
	}
	if !MayReadMail(user, c) {
		return ErrNotYours
	}
	if err := e.paused(ctx, c.ID); err != nil {
		return fmt.Errorf("%w: resume it first", err)
	}
	// Sending a draft is the plugin's send_draft: at read level for it (or
	// for all its tools) the sprig was locked down since it drafted, so the
	// draft stays for the person to discard.
	if e.mailSendLevel(ctx, c) == store.LevelRead {
		return fmt.Errorf("%s is at read level for email now, so its draft can't be sent; discard it, or raise its level first", c.DisplayName())
	}
	if err := e.DB.ResolveInbox(ctx, it.ID, user.ID); err != nil {
		return errors.New("that draft was already dealt with")
	}
	if _, err := e.Mail.SendDraft(ctx, sprig, draftID); err != nil {
		_ = e.DB.ReopenInbox(context.WithoutCancel(ctx), it.ID) // not sent: keep it in front of them
		return fmt.Errorf("couldn't send it: %s", e.Secrets.Redact(err.Error()))
	}
	e.logActivity(ctx, "info", "draft", c.ID, user.ID, fmt.Sprintf("mail draft #%d sent from needs you", it.ID), "")
	var a mailDraftAction
	_ = json.Unmarshal([]byte(it.Action), &a)
	if a.ChatRef != "" {
		if conv, err := e.DB.OpenConversation(ctx, user.ID, a.ChatRef, e.Cfg.IdleAfter); err == nil {
			_ = e.DB.AddMessage(ctx, conv.ID, "event", "", "The user sent your email draft from needs you: "+it.Title)
		}
	}
	return nil
}

// DismissInbox clears an item from "needs you". A mail draft is
// discarded from the sprig's mailbox too.
func (e *Engine) DismissInbox(ctx context.Context, user *store.User, id int64) error {
	it, err := e.DB.InboxItemByID(ctx, id)
	if err != nil {
		return err
	}
	if it.UserID != user.ID {
		return ErrNotYours
	}
	sprig, draftID, isMail := MailDraftOf(it.Action)
	if isMail && it.Status == "open" {
		// The draft goes first; the item stays in front of them until it
		// is really gone (a draft that's already gone counts).
		c, ok := e.Chars.Get(sprig)
		if ok && !MayReadMail(user, c) {
			return ErrNotYours
		}
		if ok {
			if e.Mail == nil {
				return errors.New("the mail plugin isn't running, so the draft can't be discarded yet")
			}
			if err := e.Mail.DiscardDraft(ctx, sprig, draftID); err != nil && !errors.Is(err, mail.ErrNotFound) {
				e.Log.Warn("discard mail draft", "sprig", sprig, "err", e.Secrets.Redact(err.Error()))
				return fmt.Errorf("couldn't discard the draft, so it's still here: %s", e.Secrets.Redact(err.Error()))
			}
		}
	}
	return e.DB.ResolveInbox(ctx, id, user.ID)
}

// --- the allowlist ---

// MailAllowlist returns a sprig's allowlist for someone who may see it
// (who may read its mail).
func (e *Engine) MailAllowlist(ctx context.Context, u *store.User, sprig string) ([]string, error) {
	c, ok := e.Chars.Get(sprig)
	if !ok || c.PrivateFrom(u.ID) {
		return nil, errNoSuchSprig
	}
	if !MayReadMail(u, c) {
		return nil, ErrNotYours
	}
	list, err := e.DB.MailAllow(ctx, c.ID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", store.ErrStorage, err)
	}
	return list, nil
}

// ChangeMailAllow adds and removes allowlist entries for sprig. Only its
// owner may (canManage); the change is in the trail, with an undo.
func (e *Engine) ChangeMailAllow(ctx context.Context, by *store.User, sprig string, add, remove []string, via string) ([]string, error) {
	c, ok := e.Chars.Get(sprig)
	if !ok {
		return nil, errNoSuchSprig
	}
	if c.IsKid() {
		return nil, fmt.Errorf("%s is a kid sprig: kid sprigs can't have email", c.DisplayName())
	}
	if err := e.mayManage(ctx, by, c, "change the mail allowlist"); err != nil {
		return nil, err
	}
	norm := func(in []string) ([]string, error) {
		var out []string
		for _, s := range in {
			if strings.TrimSpace(s) == "" {
				continue
			}
			v, err := mail.AllowEntry(s)
			if err != nil {
				return nil, err
			}
			if !slices.Contains(out, v) {
				out = append(out, v)
			}
		}
		return out, nil
	}
	a, err := norm(add)
	if err != nil {
		return nil, err
	}
	r, err := norm(remove)
	if err != nil {
		return nil, err
	}
	if len(a)+len(r) > 100 {
		return nil, errors.New("at most 100 changes at a time")
	}
	e.mailAllowMu.Lock()
	defer e.mailAllowMu.Unlock()
	var summary string
	_, _, err = e.DB.ChangeMailAllow(ctx, c.ID, a, r, by.ID, func(added, removed []string) *store.TrailEntry {
		var parts []string
		if len(added) > 0 {
			parts = append(parts, "added "+strings.Join(added, ", "))
		}
		if len(removed) > 0 {
			parts = append(parts, "removed "+strings.Join(removed, ", "))
		}
		summary = strings.Join(parts, "; ")
		undo := trailUndo{Type: "mailallow", Character: c.ID, Tools: removed}
		undo.Args, _ = json.Marshal(added) // undo removes what was added and puts back what was removed
		raw, _ := json.Marshal(undo)
		return &store.TrailEntry{UserID: by.ID, Character: c.ID, Kind: store.TrailAccess,
			Summary: fmt.Sprintf("%s's mail allowlist: %s (%s)", c.DisplayName(), summary, via), Undo: raw}
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", store.ErrStorage, err)
	}
	if summary != "" {
		e.logActivity(ctx, "info", "approval", c.ID, by.ID, "mail allowlist changed: "+summary, "")
	}
	list, err := e.DB.MailAllow(ctx, c.ID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", store.ErrStorage, err)
	}
	return list, nil
}

// undoMailAllow reverses an allowlist change (UndoTrail checked who).
func (e *Engine) undoMailAllow(ctx context.Context, by *store.User, u trailUndo) (string, error) {
	var added []string
	_ = json.Unmarshal(u.Args, &added)
	if _, _, err := e.DB.ChangeMailAllow(ctx, u.Character, u.Tools, added, by.ID, nil); err != nil {
		return "", err
	}
	var parts []string
	if len(added) > 0 {
		parts = append(parts, strings.Join(added, ", ")+" off the mail allowlist")
	}
	if len(u.Tools) > 0 {
		parts = append(parts, strings.Join(u.Tools, ", ")+" back on it")
	}
	return fmt.Sprintf("%s: %s", u.Character, strings.Join(parts, "; ")), nil
}

func (e *Engine) toolMailAllowlist(ctx context.Context, t *turn, args json.RawMessage) (string, error) {
	a, err := decode[struct {
		Sprig  string   `json:"sprig"`
		Add    []string `json:"add"`
		Remove []string `json:"remove"`
	}](args)
	if err != nil {
		return "", err
	}
	c, err := e.findSprig(t, a.Sprig)
	if err != nil {
		return "", err
	}
	var list []string
	if len(a.Add)+len(a.Remove) == 0 {
		if list, err = e.MailAllowlist(ctx, t.user, c.ID); err != nil {
			return "", err
		}
	} else if list, err = e.ChangeMailAllow(ctx, t.user, c.ID, a.Add, a.Remove, "in chat"); err != nil {
		return "", err
	}
	if len(list) == 0 {
		return fmt.Sprintf("%s's mail allowlist is empty: every email it writes waits in needs you for the person to send.", c.DisplayName()), nil
	}
	return fmt.Sprintf("%s may email these without asking (at act level): %s. Anything to anyone else is drafted for the person to send.", c.DisplayName(), strings.Join(list, ", ")), nil
}

// --- the address ---

var secretNameUnsafe = regexp.MustCompile(`[^a-z0-9_]`)

// mailSecret is the stored secret holding a sprig's mail password.
func mailSecret(sprig string) string {
	return "mail_" + secretNameUnsafe.ReplaceAllString(strings.ToLower(sprig), "_") + "_password"
}

func (e *Engine) toolMailAddress(ctx context.Context, t *turn, args json.RawMessage) (string, error) {
	a, err := decode[struct {
		Sprig         string `json:"sprig"`
		Action        string `json:"action"`
		Address       string `json:"address"`
		Backend       string `json:"backend"`
		NewPassword   bool   `json:"new_password"`
		RetentionDays int    `json:"retention_days"`
		SendLimit     int    `json:"send_limit"`
		mail.Settings
	}](args)
	if err != nil {
		return "", err
	}
	c, err := e.findSprig(t, a.Sprig)
	if err != nil {
		return "", err
	}
	cur, err := e.DB.MailAccount(ctx, c.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	switch a.Action {
	case "", "show":
		if !MayReadMail(t.user, c) {
			return "", ErrNotYours
		}
		if cur == nil {
			return fmt.Sprintf("%s has no email address yet.", c.DisplayName()), nil
		}
		state := "its password is stored"
		if !e.Secrets.Has(ctx, cur.SecretName) {
			state = "waiting for its password (ask the owner to send it in a DM after calling this with action set again)"
		}
		on := "the mail plugin is running"
		if !e.mailOn(ctx) {
			on = "the mail plugin isn't running (docker compose --profile mail up -d)"
		}
		return fmt.Sprintf("%s's address is %s (%s): %s; mail older than %d days isn't offered to it; it may send %d a day on its own; %s.",
			c.DisplayName(), cur.Address, cur.Backend, state, cur.RetentionDays, cur.SendLimit, on), nil
	case "remove":
		if err := e.mayManage(ctx, t.user, c, "remove its email address"); err != nil {
			return "", err
		}
		if cur == nil {
			return fmt.Sprintf("%s has no email address.", c.DisplayName()), nil
		}
		if err := e.DB.DeleteMailAccount(ctx, c.ID); err != nil {
			return "", err
		}
		_ = e.Secrets.Delete(ctx, cur.SecretName)
		e.addTrail(ctx, t.user.ID, c.ID, store.TrailAccess, fmt.Sprintf("%s's email address %s removed from Thicket (the mailbox itself is untouched)", c.DisplayName(), cur.Address), "", nil)
		e.NudgeMail()
		return fmt.Sprintf("Done: Thicket no longer uses %s for %s. The mailbox itself is still there at your provider.", cur.Address, c.DisplayName()), nil
	case "set":
	default:
		return "", errors.New("action must be show, set or remove")
	}
	if c.IsKid() {
		e.refused(ctx, t.user, c.ID, "tried to give a kid sprig an email address")
		return "", fmt.Errorf("%s is a kid sprig: kid sprigs can't have email", c.DisplayName())
	}
	if err := e.mayManage(ctx, t.user, c, "set up its email address"); err != nil {
		return "", err
	}
	if !t.isDM {
		return "", errors.New("an email address is set up in a DM (its password comes next); ask the user to DM you")
	}
	addr, err := mail.Address(a.Address)
	if err != nil {
		return "", err
	}
	backend := strings.ToLower(strings.TrimSpace(a.Backend))
	if backend == "" {
		backend = "jmap"
	}
	s := a.Settings
	if backend == "imap" {
		s.Defaults()
		if s.Username == "" {
			s.Username = addr
		}
	}
	if err := s.Check(backend); err != nil {
		return "", err
	}
	if a.RetentionDays == 0 {
		a.RetentionDays = 90
	}
	if a.SendLimit == 0 {
		a.SendLimit = 20
	}
	if a.RetentionDays < 1 || a.RetentionDays > 3650 || a.SendLimit < 1 || a.SendLimit > 500 {
		return "", errors.New("retention_days is 1-3650 and send_limit 1-500")
	}
	raw, _ := json.Marshal(s)
	acct := store.MailAccount{Sprig: c.ID, Address: addr, Backend: backend, Settings: string(raw), SecretName: mailSecret(c.ID),
		RetentionDays: a.RetentionDays, SendLimit: a.SendLimit, CreatedBy: t.user.ID}
	// A different server, login or address, or a new password asked for:
	// the old password is dropped first, so the new settings are never
	// tried with it (the address waits until the new one arrives). Only
	// retention and the daily limit keep it.
	newLogin := cur != nil && (cur.Address != addr || cur.Backend != backend || cur.Settings != string(raw))
	if newLogin || a.NewPassword {
		if err := e.Secrets.Delete(ctx, acct.SecretName); err != nil {
			return "", fmt.Errorf("couldn't drop the old password: %w", err)
		}
	}
	if err := e.DB.PutMailAccount(ctx, acct); err != nil {
		return "", err
	}
	e.addTrail(ctx, t.user.ID, c.ID, store.TrailAccess, fmt.Sprintf("%s's email address set to %s (%s); retention %d days, %d sends a day", c.DisplayName(), addr, backend, a.RetentionDays, a.SendLimit), "", nil)
	e.NudgeMail()
	var next []string
	if cur == nil || cur.SecretName != acct.SecretName || a.NewPassword || !e.Secrets.Has(ctx, acct.SecretName) {
		if err := e.DB.SetPendingSecret(ctx, store.PendingSecret{UserID: t.user.ID, Name: acct.SecretName, Purpose: "app password for " + addr}); err != nil {
			return "", err
		}
		next = append(next, fmt.Sprintf("Ask the user to send the app password for %s as their next message here (or 'cancel'). Thicket captures and encrypts it; you never see it. Tell them to make an app password at their provider rather than give their main one.", addr))
	}
	if !slices.Contains(c.Tools, "mcp:mail/*") {
		next = append(next, fmt.Sprintf("%s isn't granted the mail tools yet: grant_tools with mcp:mail/* (only if they want that).", c.DisplayName()))
	}
	if !e.mailOn(ctx) {
		next = append(next, "The mail plugin isn't running: they start it with `docker compose --profile mail up -d`.")
	}
	next = append(next, "Its mail allowlist (who it may email without asking) is changed with mail_allowlist; until then every email it writes is a draft for them to send.")
	return fmt.Sprintf("Saved: %s's address is %s.\n%s", c.DisplayName(), addr, strings.Join(next, "\n")), nil
}

// --- keeping the plugin current, and what happens in mailboxes ---

// MailAccounts builds what the plugin should serve: every address whose
// sprig still exists, isn't a kid sprig and whose password is stored.
func (e *Engine) MailAccounts(ctx context.Context) ([]mail.Account, error) {
	rows, err := e.DB.MailAccounts(ctx)
	if err != nil {
		return nil, err
	}
	out := []mail.Account{}
	for _, r := range rows {
		c, ok := e.Chars.Get(r.Sprig)
		if !ok || c.IsKid() {
			continue
		}
		pw, err := e.Secrets.Get(ctx, r.SecretName)
		if err != nil || pw == "" {
			continue
		}
		var s mail.Settings
		_ = json.Unmarshal([]byte(r.Settings), &s)
		a := mail.Account{Sprig: r.Sprig, Address: r.Address, Name: c.DisplayName(), Backend: r.Backend, Username: s.Username, Password: pw,
			RetentionDays: r.RetentionDays, SendLimit: r.SendLimit}
		if r.Backend == "jmap" {
			a.JMAP = &mail.Server{SessionURL: s.SessionURL}
		} else {
			a.IMAP = &mail.Server{Host: s.IMAPHost, Port: s.IMAPPort, Security: s.IMAPSecurity}
			a.SMTP = &mail.Server{Host: s.SMTPHost, Port: s.SMTPPort, Security: s.SMTPSecurity}
		}
		out = append(out, a)
	}
	return out, nil
}

// NudgeMail has the plugin's accounts pushed again soon (an address or
// password changed).
func (e *Engine) NudgeMail() {
	select {
	case e.mailNudge() <- struct{}{}:
	default:
	}
}

func (e *Engine) mailNudge() chan struct{} {
	e.mailNudgeOnce.Do(func() { e.mailNudgeCh = make(chan struct{}, 1) })
	return e.mailNudgeCh
}

// mailSyncEvery is how often accounts are pushed even with no change (a
// restarted plugin has none); tests shorten it.
var mailSyncEvery = 30 * time.Second

// RunMail keeps the mail plugin's accounts current and follows what
// happens in sprigs' mailboxes, until ctx ends. The app starts it when
// the plugin is configured.
func (e *Engine) RunMail(ctx context.Context) {
	if e.Mail == nil {
		return
	}
	// Everything started here ends before RunMail returns: following the
	// events, and the trigger polls new mail starts.
	defer e.mailWork.Wait()
	e.mailWork.Go(func() { e.followMail(ctx) })
	t := time.NewTicker(mailSyncEvery)
	defer t.Stop()
	for {
		e.syncMail(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-e.mailNudge():
		}
	}
}

// mailOn reports whether the mail plugin is connected (its integration
// set up and on): until then there's nothing to keep current.
func (e *Engine) mailOn(ctx context.Context) bool { return e.managedPlugins(ctx)[mail.Plugin] }

func (e *Engine) syncMail(ctx context.Context) {
	if !e.mailOn(ctx) {
		return
	}
	accts, err := e.MailAccounts(ctx)
	if err != nil {
		e.Log.Error("mail accounts", "err", err)
		return
	}
	for _, a := range accts {
		e.Secrets.Remember(a.Password) // never logged or shown, even in a plugin's error
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	states, err := e.Mail.PutAccounts(cctx, accts)
	if err != nil {
		if ctx.Err() == nil {
			e.Log.Debug("mail plugin not reachable", "err", e.Secrets.Redact(err.Error()))
		}
		return
	}
	for _, s := range states {
		if s.State == "error" {
			e.Log.Warn("mail account", "sprig", s.Sprig, "err", e.Secrets.Redact(s.Error))
		}
	}
}

// mailCursorKey keeps how far the plugin's events were read, so a
// restart doesn't trail them twice.
const mailCursorKey = "mail.events_cursor"

func (e *Engine) followMail(ctx context.Context) {
	epoch, after := "", int64(0)
	if v, ok, _ := e.DB.Setting(ctx, mailCursorKey); ok {
		if ep, seq, found := strings.Cut(v, ":"); found {
			epoch = ep
			after, _ = strconv.ParseInt(seq, 10, 64)
		}
	}
	backoff := time.Second
	for ctx.Err() == nil {
		if !e.mailOn(ctx) {
			select {
			case <-ctx.Done():
				return
			case <-time.After(mailSyncEvery):
			}
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 40*time.Second)
		evs, err := e.Mail.Events(cctx, after, 25*time.Second)
		cancel()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, time.Minute)
			continue
		}
		backoff = time.Second
		if evs.Epoch != epoch {
			// The plugin restarted: its numbering starts again, and what
			// it has from before was never seen here.
			if epoch != "" && after > 0 {
				epoch, after = evs.Epoch, 0
				continue
			}
			epoch = evs.Epoch
		}
		failed := false
		for _, ev := range evs.Events {
			if ev.Seq <= after {
				continue
			}
			if err := e.MailEvent(ctx, ev); err != nil {
				// Not recorded: stop here and offer it again, so the trail
				// never misses a message.
				e.Log.Error("mail event not recorded; retrying", "seq", ev.Seq, "err", err)
				failed = true
				break
			}
			after = ev.Seq
		}
		if !failed && evs.Next > after {
			after = evs.Next
		}
		_ = e.DB.SetSetting(ctx, mailCursorKey, fmt.Sprintf("%s:%d", epoch, after))
		if failed {
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, time.Minute)
		}
	}
}

// MailEvent handles one thing that happened in a sprig's mailbox. An
// error means it wasn't fully recorded: followMail stops there and offers
// it again (what was recorded is written so a retry doesn't duplicate it:
// the code item is keyed by message id, and goes first).
func (e *Engine) MailEvent(ctx context.Context, ev mail.Event) error {
	c, ok := e.Chars.Get(ev.Sprig)
	if !ok {
		return nil
	}
	owner := e.mailOwner(ctx, c.ID)
	trail := func(summary, detail string) error {
		_, err := e.DB.AddTrailID(ctx, store.TrailEntry{UserID: owner, Character: c.ID, Kind: store.TrailMail, Summary: summary, Detail: detail})
		return err
	}
	clean := func(s string) string { return truncate(strings.Join(strings.Fields(e.Secrets.Redact(s)), " "), 200) }
	detail := func() string {
		var b strings.Builder
		fmt.Fprintf(&b, "from: %s\nto: %s", clean(ev.From), clean(strings.Join(ev.To, ", ")))
		if len(ev.Cc) > 0 {
			fmt.Fprintf(&b, "\ncc: %s", clean(strings.Join(ev.Cc, ", ")))
		}
		if len(ev.Bcc) > 0 {
			fmt.Fprintf(&b, "\nbcc: %s", clean(strings.Join(ev.Bcc, ", ")))
		}
		fmt.Fprintf(&b, "\nsubject: %s\nsize: %d bytes\nmessage id: %s", clean(ev.Subject), ev.Size, clean(ev.MessageID))
		return b.String()
	}
	switch ev.Kind {
	case "received":
		if ev.OTP && owner != 0 {
			if _, err := e.DB.AddInbox(ctx, store.InboxItem{UserID: owner, Character: c.ID, Kind: store.InboxCode, Ref: "mail:" + c.ID + ":" + ev.MessageID,
				Title: truncate(fmt.Sprintf("A one-time code arrived for %s from %s", c.DisplayName(), clean(ev.From)), 200),
				Detail: fmt.Sprintf("Subject: %s\n\nOpen %s's mailbox on the dashboard to read it. %s sees the code hidden and never types it in anywhere: if it's for a sign-in it was doing, you enter it.",
					clean(ev.Subject), c.DisplayName(), c.DisplayName())}); err != nil {
				return err
			}
		}
		if err := trail(fmt.Sprintf("%s received mail from %s: %s", c.DisplayName(), clean(ev.From), clean(ev.Subject)), detail()); err != nil {
			return err
		}
		e.mailWork.Go(func() { e.PollTriggersFor(context.WithoutCancel(ctx), mail.Plugin, c.ID) })
	case "sent":
		who := c.DisplayName() + " sent"
		if ev.Approved {
			who = "Sent from " + c.DisplayName() + "'s address (approved by a person)"
		}
		return trail(fmt.Sprintf("%s mail to %s: %s", who, clean(strings.Join(ev.To, ", ")), clean(ev.Subject)), detail())
	case "error":
		e.logActivity(ctx, "warn", "tool", c.ID, owner, "mail: "+clean(ev.Detail), "")
	}
	return nil
}

// PollTriggersFor polls, now, a sprig's triggers on one integration (new
// mail arrived), whatever their interval.
func (e *Engine) PollTriggersFor(ctx context.Context, server, sprig string) {
	if e.MCP == nil || e.Fire == nil {
		return
	}
	trigs, err := e.DB.PollTriggers(ctx)
	if err != nil {
		return
	}
	for _, tr := range trigs {
		if tr.Server != server {
			continue
		}
		if j, err := e.DB.JobByID(ctx, tr.JobID); err != nil || j.Character != sprig {
			continue
		}
		e.pollOne(ctx, tr)
	}
}

// --- the dashboard's mailbox ---

// CanReadMail reports whether u may open sprig's mailbox (MayReadMail).
func (e *Engine) CanReadMail(u *store.User, sprig string) bool {
	c, ok := e.Chars.Get(sprig)
	return ok && MayReadMail(u, c)
}

// CanManageMail reports whether u may change sprig's mail allowlist.
func (e *Engine) CanManageMail(u *store.User, sprig string) bool {
	c, ok := e.Chars.Get(sprig)
	return ok && !c.IsKid() && canManage(u, c)
}

// SendMailDraft sends a draft from a sprig's mailbox, for a person who
// may read it (as Send in "needs you"); its "needs you" item is cleared.
func (e *Engine) SendMailDraft(ctx context.Context, u *store.User, sprig, id string) error {
	if e.Mail == nil {
		return errors.New("the mail plugin isn't running")
	}
	c, ok := e.Chars.Get(sprig)
	if !ok || !MayReadMail(u, c) {
		return errNoSuchSprig
	}
	if err := e.paused(ctx, c.ID); err != nil {
		return fmt.Errorf("%w: resume it first", err)
	}
	if _, err := e.Mail.SendDraft(ctx, c.ID, id); err != nil {
		return errors.New(e.Secrets.Redact(err.Error()))
	}
	e.logActivity(ctx, "info", "draft", c.ID, u.ID, "mail draft sent from the mailbox", "")
	e.clearMailDraftItems(ctx, c.ID, id)
	return nil
}

// DiscardMailDraft deletes a draft from a sprig's mailbox.
func (e *Engine) DiscardMailDraft(ctx context.Context, u *store.User, sprig, id string) error {
	if e.Mail == nil {
		return errors.New("the mail plugin isn't running")
	}
	c, ok := e.Chars.Get(sprig)
	if !ok || !MayReadMail(u, c) {
		return errNoSuchSprig
	}
	if err := e.Mail.DiscardDraft(ctx, c.ID, id); err != nil {
		return errors.New(e.Secrets.Redact(err.Error()))
	}
	e.clearMailDraftItems(ctx, c.ID, id)
	return nil
}

// clearMailDraftItems resolves the "needs you" items for a draft dealt
// with from the mailbox.
func (e *Engine) clearMailDraftItems(ctx context.Context, sprig, draftID string) {
	items, err := e.DB.OpenDrafts(ctx, sprig)
	if err != nil {
		return
	}
	for _, it := range items {
		if s, d, ok := MailDraftOf(it.Action); ok && s == sprig && d == draftID {
			_ = e.DB.ResolveInbox(ctx, it.ID, 0)
		}
	}
}
