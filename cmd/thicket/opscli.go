package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/theshield2594/thicket/internal/app"
	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/doctor"
	"github.com/theshield2594/thicket/internal/llm"
	"github.com/theshield2594/thicket/internal/settings"
	"github.com/theshield2594/thicket/internal/store"
)

// --- costs ---

func costsCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("costs", flag.ContinueOnError)
	_ = fs.Bool("week", false, "the last 7 days (the default)")
	month := fs.Bool("month", false, "this calendar month (UTC)")
	days := fs.Int("days", 0, "the last N days")
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return errors.New("usage: thicket costs [--week|--month|--days N] [--json]")
	}
	cfg := loadConfig()
	db, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	return printCosts(context.Background(), db, settings.New(db, cfg), costsSince(time.Now(), *month, *days), *asJSON, out)
}

// costsSince picks the window: --month, --days N, else the last 7 days.
func costsSince(now time.Time, month bool, days int) time.Time {
	now = now.UTC()
	switch {
	case month:
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	case days > 0:
		return now.AddDate(0, 0, -days)
	}
	return now.AddDate(0, 0, -7)
}

func printCosts(ctx context.Context, db *store.Store, st *settings.Settings, since time.Time, asJSON bool, out io.Writer) error {
	total, err := db.UsageSummary(ctx, since)
	if err != nil {
		return err
	}
	bySprig, err := db.UsageSummary(ctx, since, "character", "origin")
	if err != nil {
		return err
	}
	byModel, err := db.UsageSummary(ctx, since, "model")
	if err != nil {
		return err
	}
	byDay, err := db.UsageSummary(ctx, since, "day")
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	monthSpent, err := db.SpendSince(ctx, monthStart, "")
	if err != nil {
		return err
	}
	monthCap := st.Float(ctx, "budget.monthly_usd")

	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{
			"since": since, "total": total, "by_sprig_and_origin": bySprig, "by_model": byModel, "by_day": byDay,
			"month_spent_usd": monthSpent, "month_cap_usd": monthCap,
		})
	}

	t := store.UsageRow{}
	if len(total) == 1 {
		t = total[0]
	}
	fmt.Fprintf(out, "Model spend since %s UTC: $%.4f over %d calls (%s tokens in, %s out)\n",
		since.Format("2006-01-02 15:04"), t.CostUSD, t.Calls, humanInt(t.PromptTokens), humanInt(t.CompletionTokens))
	if monthCap > 0 {
		fmt.Fprintf(out, "This month: $%.2f of the $%.2f cap (%.0f%%)", monthSpent, monthCap, 100*monthSpent/monthCap)
		switch {
		case monthSpent >= monthCap:
			fmt.Fprint(out, ": cap reached, heartbeats are paused")
		case monthSpent >= 0.8*monthCap:
			fmt.Fprint(out, ": over 80%")
		}
		fmt.Fprintln(out)
	} else {
		fmt.Fprintf(out, "This month: $%.2f (no monthly cap)\n", monthSpent)
	}
	if t.Calls == 0 {
		return nil
	}

	// Sprig × origin as a small pivot.
	type split struct{ heartbeat, task, other float64 }
	sprigs := map[string]*split{}
	var names []string
	for _, r := range bySprig {
		name := orNone(r.Keys[0])
		s := sprigs[name]
		if s == nil {
			s = &split{}
			sprigs[name] = s
			names = append(names, name)
		}
		switch r.Keys[1] {
		case llm.OriginHeartbeat:
			s.heartbeat += r.CostUSD
		case llm.OriginTask:
			s.task += r.CostUSD
		default:
			s.other += r.CostUSD
		}
	}
	sort.SliceStable(names, func(i, j int) bool {
		a, b := sprigs[names[i]], sprigs[names[j]]
		return a.heartbeat+a.task+a.other > b.heartbeat+b.task+b.other
	})
	fmt.Fprintln(out)
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "sprig\theartbeat\ttask\ttotal\t")
	for _, n := range names {
		s := sprigs[n]
		fmt.Fprintf(tw, "%s\t$%.4f\t$%.4f\t$%.4f\t\n", n, s.heartbeat, s.task+s.other, s.heartbeat+s.task+s.other)
	}
	tw.Flush()

	fmt.Fprintln(out)
	tw = tabwriter.NewWriter(out, 0, 4, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "model\tcalls\ttokens\tcost\t")
	for _, r := range byModel {
		fmt.Fprintf(tw, "%s\t%d\t%s\t$%.4f\t\n", orNone(r.Keys[0]), r.Calls, humanInt(r.PromptTokens+r.CompletionTokens), r.CostUSD)
	}
	tw.Flush()

	fmt.Fprintln(out)
	tw = tabwriter.NewWriter(out, 0, 4, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "day (UTC)\tcalls\tcost\t")
	for _, r := range byDay {
		fmt.Fprintf(tw, "%s\t%d\t$%.4f\t\n", r.Keys[0], r.Calls, r.CostUSD)
	}
	tw.Flush()
	fmt.Fprintln(out, "\nheartbeat = jobs, check-ins, follow-ups and memory review; task = chats and work you asked for.")
	fmt.Fprintln(out, "Costs are what OpenRouter reported per call; local model servers count as $0.")
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func humanInt(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 10_000:
		return fmt.Sprintf("%.0fk", float64(n)/1e3)
	}
	return strconv.Itoa(n)
}

// --- doctor ---

func doctorCmd(out io.Writer) error {
	cfg := loadConfig()
	d := &doctor.Doctor{Cfg: cfg}
	checks := d.Run(context.Background())
	failed := 0
	for _, c := range checks {
		mark := "ok  "
		switch {
		case !c.OK && c.Optional:
			mark = "warn"
		case !c.OK:
			mark = "FAIL"
			failed++
		}
		fmt.Fprintf(out, "[%s] %-16s %s\n", mark, c.Name, c.Detail)
	}
	if failed > 0 {
		return fmt.Errorf("%d check(s) failed", failed)
	}
	return nil
}

// --- voice-check ---

func voiceCheckCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("voice-check", flag.ContinueOnError)
	record := fs.Bool("record", false, "save the current reply as the reference for tests that have none")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: thicket voice-check SPRIG [--record]")
	}
	cfg := loadConfig()
	ctx := context.Background()
	a, err := app.New(ctx, cfg, version, app.Options{NoHTTP: true, LogOut: io.Discard})
	if err != nil {
		return err
	}
	defer a.Close()
	c, ok := a.Chars.Get(strings.ToLower(pos[0]))
	if !ok {
		if c, ok = a.Chars.FindByName(pos[0]); !ok {
			return fmt.Errorf("no sprig called %q", pos[0])
		}
	}
	if !a.Engine.ModelReady(ctx) {
		return errors.New("no model is set up yet: DM Juni first")
	}
	return runVoiceCheck(ctx, a, c, *record, out)
}

func runVoiceCheck(ctx context.Context, a *app.App, c *character.Character, record bool, out io.Writer) error {
	tests, err := character.LoadVoiceTests(a.Chars.Dir(), c.ID)
	if err != nil {
		return err
	}
	if len(tests) == 0 {
		return fmt.Errorf("%s has no voice tests: add YAML files (prompt, reference, notes) to %s", c.DisplayName(), character.VoiceTestDir(a.Chars.Dir(), c.ID))
	}
	fmt.Fprintf(out, "Voice check for %s: %d test(s), model %s, judge %s\n\n", c.DisplayName(), len(tests),
		orDefaultStr(c.Model, a.Settings.String(ctx, "model")), a.Settings.String(ctx, "cheap_model"))
	drifted, broken := 0, 0
	for _, vt := range tests {
		reply, err := a.Engine.VoiceReply(ctx, c, vt.Prompt)
		if err != nil {
			broken++
			fmt.Fprintf(out, "ERROR  %s: couldn't get a reply: %v\n", vt.Name, err)
			continue
		}
		if vt.Reference == "" {
			if record {
				vt.Reference = reply
				if err := character.SaveVoiceTest(vt); err != nil {
					return err
				}
				fmt.Fprintf(out, "SAVED  %s: recorded the current reply as the reference\n       %s\n", vt.Name, oneLine(reply))
			} else {
				fmt.Fprintf(out, "SKIP   %s: no reference yet (run with --record to save the current reply)\n", vt.Name)
			}
			continue
		}
		v, err := a.Engine.JudgeVoice(ctx, c, vt, reply)
		if err != nil {
			broken++
			fmt.Fprintf(out, "ERROR  %s: the judge failed: %v\n", vt.Name, err)
			continue
		}
		if v.Drifted() {
			drifted++
			fmt.Fprintf(out, "DRIFT  %s: %s. %s\n       you: %s\n       ref: %s\n       now: %s\n", vt.Name, strings.Join(v.Failed(), ", "), v.Note,
				oneLine(vt.Prompt), oneLine(vt.Reference), oneLine(reply))
		} else {
			fmt.Fprintf(out, "ok     %s\n", vt.Name)
		}
	}
	fmt.Fprintf(out, "\n%d of %d in voice", len(tests)-drifted-broken, len(tests))
	if drifted+broken > 0 {
		fmt.Fprintf(out, ", %d drifted, %d errors\n", drifted, broken)
		return errors.New("voice drift found")
	}
	fmt.Fprintln(out)
	return nil
}

func orDefaultStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 220 {
		s = s[:220] + "…"
	}
	return s
}

// --- inbox ---

func inboxCmd(args []string, out io.Writer) error {
	cfg := loadConfig()
	db, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx := context.Background()
	if len(args) == 0 || args[0] == "list" {
		items, err := db.OpenInbox(ctx, 0)
		if err != nil {
			return err
		}
		apps, err := db.PendingApprovals(ctx, 0, 30*time.Minute)
		if err != nil {
			return err
		}
		if len(items) == 0 && len(apps) == 0 {
			fmt.Fprintln(out, "Nothing needs you.")
			return nil
		}
		for _, a := range apps {
			fmt.Fprintf(out, "approval  %s wants to run %s (user %d): answer approve/deny in chat\n", a.Character, a.Tool, a.UserID)
		}
		for _, it := range items {
			n := ""
			if it.Count > 1 {
				n = fmt.Sprintf(" ×%d", it.Count)
			}
			fmt.Fprintf(out, "#%-6d %-11s %s%s  (user %d, %s)\n", it.ID, it.Kind, it.Title, n, it.UserID, it.UpdatedAt.Format("2006-01-02 15:04"))
			if d := oneLine(it.Detail); d != "" {
				fmt.Fprintf(out, "        %s\n", d)
			}
		}
		return nil
	}
	if args[0] == "resolve" && len(args) == 2 {
		id, err := strconv.ParseInt(strings.TrimPrefix(args[1], "#"), 10, 64)
		if err != nil {
			return err
		}
		if err := db.ResolveInbox(ctx, id, 0); errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("no open inbox item #%d", id)
		} else if err != nil {
			return err
		}
		fmt.Fprintf(out, "resolved #%d\n", id)
		return nil
	}
	return errors.New("usage: thicket inbox [list] | resolve ID")
}

// --- migrate ---

func migrateCmd(out io.Writer) error {
	cfg := loadConfig()
	db, err := openStore(cfg) // opening applies pending migrations, with a snapshot first
	if err != nil {
		return err
	}
	defer db.Close()
	applied, err := db.AppliedMigrations(context.Background())
	if err != nil {
		return err
	}
	known, err := store.KnownMigrations()
	if err != nil {
		return err
	}
	for _, m := range applied {
		fmt.Fprintf(out, "  %04d  %-36s applied %s\n", m.Version, m.Name, m.AppliedAt.UTC().Format("2006-01-02 15:04"))
	}
	fmt.Fprintf(out, "schema version %d of %d: up to date\n", len(applied), known)
	return nil
}
