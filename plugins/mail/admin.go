package main

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The dashboard's mailbox view: a person looking at the sprig's mail. No
// retention filter, one-time codes shown.

// adminAccount finds the account and its connection, or answers the error.
func (s *server) adminAccount(w http.ResponseWriter, r *http.Request) (*account, Backend, bool) {
	a := s.lookup(r.PathValue("sprig"))
	if a == nil {
		// Not 404: after a plugin restart no account exists until core pushes
		// them again, and a 404 would tell core the draft is gone.
		jsonError(w, http.StatusServiceUnavailable, "that sprig's mail account isn't connected yet (or isn't set up)")
		return nil, nil, false
	}
	be, err := a.ready()
	if err != nil {
		jsonError(w, http.StatusServiceUnavailable, err.Error())
		return nil, nil, false
	}
	return a, be, true
}

// adminFail answers an error. 404 means the draft or message is gone
// (core drops its "needs you" item), 503 the account is unavailable (see
// adminAccount); anything else is a failure to show.
func (s *server) adminFail(w http.ResponseWriter, a *account, err error) {
	code := http.StatusBadGateway
	switch {
	case errors.Is(err, errNotDraft), errors.Is(err, errNotFound), errors.Is(err, errDraftSent):
		code = http.StatusNotFound
	case errors.Is(err, errDraftBusy):
		code = http.StatusConflict
	case errors.Is(err, errBadDraft):
		code = http.StatusBadRequest
	}
	jsonError(w, code, a.cfg.scrub(err))
}

func (s *server) mailboxList(w http.ResponseWriter, r *http.Request) {
	a, be, ok := s.adminAccount(w, r)
	if !ok {
		return
	}
	folder := r.URL.Query().Get("folder")
	if folder == "" {
		folder = "inbox"
	}
	if !folders[folder] {
		jsonError(w, http.StatusBadRequest, "folder must be inbox, drafts, sent or archive")
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 50
	}
	limit = min(limit, 200)
	list, err := be.List(r.Context(), folder, limit, false, time.Time{})
	if err != nil {
		s.adminFail(w, a, err)
		return
	}
	out := []map[string]any{}
	for _, m := range list {
		out = append(out, map[string]any{
			"id": m.ID, "from": m.From.String(), "to": addrStrings(m.To), "subject": m.Subject, "date": rfc3339(m.Date),
			"size": m.Size, "preview": m.Preview, "attachments": m.Attachments, "unread": m.Unread, "draft": m.Draft,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": out})
}

func (s *server) mailboxMessage(w http.ResponseWriter, r *http.Request) {
	a, be, ok := s.adminAccount(w, r)
	if !ok {
		return
	}
	m, err := be.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		s.adminFail(w, a, err)
		return
	}
	atts := m.AttachmentList
	if atts == nil {
		atts = []Attachment{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": m.ID, "from": m.From.String(), "to": addrStrings(m.To), "cc": addrStrings(m.Cc), "bcc": addrStrings(m.Bcc),
		"subject": m.Subject, "date": rfc3339(m.Date), "size": m.Size, "text": m.Text, "draft": m.Draft, "attachments": atts,
	})
}

func (s *server) mailboxAttachment(w http.ResponseWriter, r *http.Request) {
	a, be, ok := s.adminAccount(w, r)
	if !ok {
		return
	}
	att, data, err := be.Attachment(r.Context(), r.PathValue("id"), r.PathValue("aid"))
	if err != nil {
		s.adminFail(w, a, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Mail-Filename", headerSafe(att.Name))
	w.Header().Set("X-Mail-Type", headerSafe(att.Type))
	_, _ = w.Write(data)
}

// headerSafe keeps a value printable ASCII so it can't break a header.
func headerSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return '_'
		}
		return r
	}, s)
}

// mailboxSend is a person sending a draft: approved.
func (s *server) mailboxSend(w http.ResponseWriter, r *http.Request) {
	a, be, ok := s.adminAccount(w, r)
	if !ok {
		return
	}
	_, msgID, _, err := s.sendDraftCore(r.Context(), a, be, r.PathValue("id"), policy{approved: true}, false)
	if err != nil {
		s.adminFail(w, a, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"message_id": bracket(msgID)})
}

func (s *server) mailboxDiscard(w http.ResponseWriter, r *http.Request) {
	a, be, ok := s.adminAccount(w, r)
	if !ok {
		return
	}
	if err := s.discard(r.Context(), a, be, r.PathValue("id")); err != nil {
		s.adminFail(w, a, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

// doctor checks every account: the backend's own checks plus DNS advice.
func (s *server) doctor(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	list := make([]*account, 0, len(s.accounts))
	for _, a := range s.accounts {
		list = append(list, a)
	}
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	out := make([]map[string]any, len(list))
	var wg sync.WaitGroup
	for i, a := range list {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var checks []Check
			if state, msg := a.status(); state != "connected" {
				checks = append(checks, Check{Name: "session", OK: false, Detail: state + ": " + msg})
			} else {
				checks = a.be.Checks(ctx)
			}
			for i := range checks {
				checks[i].Detail = a.cfg.scrub(errors.New(checks[i].Detail))
			}
			checks = append(checks, s.dnsCheck(ctx, a.cfg.Address))
			out[i] = map[string]any{"sprig": a.cfg.Sprig, "address": a.cfg.Address, "checks": checks}
		}()
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

// dkimSelectors are tried in turn; providers use different ones.
var dkimSelectors = []string{"fm1", "default", "google", "selector1"}

// dnsCheck looks for SPF, DKIM and DMARC on the sending domain. Missing
// records are advice (warn), never a failure.
func (s *server) dnsCheck(ctx context.Context, address string) Check {
	domain := address[strings.LastIndexByte(address, '@')+1:]
	has := func(name, prefix string) bool {
		recs, err := s.resolver.LookupTXT(ctx, name)
		if err != nil {
			return false
		}
		for _, t := range recs {
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(t)), prefix) {
				return true
			}
		}
		return false
	}
	var missing []string
	if !has(domain, "v=spf1") {
		missing = append(missing, "no SPF record for "+domain)
	}
	dkim := ""
	for _, sel := range dkimSelectors {
		recs, err := s.resolver.LookupTXT(ctx, sel+"._domainkey."+domain)
		if err == nil && len(recs) > 0 && (strings.Contains(strings.Join(recs, ""), "v=DKIM1") || strings.Contains(strings.Join(recs, ""), "p=")) {
			dkim = sel
			break
		}
	}
	if dkim == "" {
		missing = append(missing, "no DKIM record found for "+domain+" (tried "+strings.Join(dkimSelectors, ", ")+")")
	}
	if !has("_dmarc."+domain, "v=dmarc1") {
		missing = append(missing, "no DMARC record for "+domain)
	}
	if len(missing) > 0 {
		return Check{Name: "dns", OK: true, Warn: true, Detail: strings.Join(missing, "; ")}
	}
	return Check{Name: "dns", OK: true, Detail: "SPF, DKIM (" + dkim + ") and DMARC found for " + domain}
}
