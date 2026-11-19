package agent

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/theshield2594/thicket/internal/avatar"
	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/sprigpkg"
	"github.com/theshield2594/thicket/internal/store"

	"gopkg.in/yaml.v3"
)

// Starter sprigs are a few ready-made sprigs for a household, shipped in
// the binary as sprig packages (the export-sprig format). Each one's
// source is a folder in startersprigs/, readable in review: the
// character file, avatar.json, voice tests, the skills it needs and its
// suggested routines, plus starter.yaml for what a package can't say
// (which tiers it may be added as, its levels and spend cap, and when Juni
// should offer it). The folder is assembled into a package with the
// sprigpkg writer and read back with the package reader, so a starter goes
// through the same checks as any shared sprig.
//
// Nothing is added automatically: `thicket sprig starter` or Juni in chat
// (starter_sprigs) shows everything first. Suggested routines arrive
// paused, and kid starters become kid sprigs only through a parent.
//
//go:embed startersprigs
var starterSprigFS embed.FS

// StarterSprig is one bundled sprig.
type StarterSprig struct {
	// ID names the starter (homework-helper); the sprig has its own id.
	ID      string
	Summary string
	// Tiers it may be added as; the first is the default.
	Tiers []string
	// Ages suits a kid starter ("5-12").
	Ages string
	// Offer says when Juni should suggest it.
	Offer string
	// DailyUSD is its spend cap (budget.character.<id>.daily_usd).
	DailyUSD float64
	// Levels are set on top of draft for every granted tool (tool "*"
	// overrides that). Kid sprigs are read only whatever is stored.
	Levels map[string]string
	// Char is the packaged character (family tier). Don't change it.
	Char *character.Character
	// Package is the .sprig file.
	Package []byte
	// VoiceTests are the package's voice tests, in file order.
	VoiceTests []character.VoiceTest
	Skills     []store.Skill
	Routines   []sprigpkg.Routine
	// CheckIns are its suggested check-in presets: off until the person
	// accepts them (StarterOptions.CheckIns, or set_initiative later).
	CheckIns StarterCheckIns
}

// StarterCheckIns are the check-in presets a starter suggests (see
// checkins.go): a morning brief, heads-ups before calendar events,
// reminders when something noted is due.
type StarterCheckIns struct {
	MorningBrief   string `yaml:"morning_brief"`
	EventReminders int    `yaml:"event_reminders"`
	DueReminders   bool   `yaml:"due_reminders"`
}

// Any reports whether it suggests any.
func (k StarterCheckIns) Any() bool {
	return k.MorningBrief != "" || k.EventReminders > 0 || k.DueReminders
}

// String describes them for people.
func (k StarterCheckIns) String() string {
	var p []string
	if k.MorningBrief != "" {
		p = append(p, "a morning brief at "+k.MorningBrief)
	}
	if k.EventReminders > 0 {
		p = append(p, fmt.Sprintf("a heads-up %d minutes before calendar events", k.EventReminders))
	}
	if k.DueReminders {
		p = append(p, "reminders when something noted is due")
	}
	return strings.Join(p, ", ")
}

func (k StarterCheckIns) check() []string {
	var probs []string
	if k.MorningBrief != "" {
		if _, err := character.ParseClock(k.MorningBrief); err != nil {
			probs = append(probs, "check_ins.morning_brief: "+err.Error())
		}
	}
	if n := k.EventReminders; n != 0 && (n < character.MinEventReminders || n > character.MaxEventReminders) {
		probs = append(probs, fmt.Sprintf("check_ins.event_reminders is minutes before an event, %d-%d", character.MinEventReminders, character.MaxEventReminders))
	}
	return probs
}

type starterMeta struct {
	Summary  string            `yaml:"summary"`
	Tiers    []string          `yaml:"tiers"`
	Ages     string            `yaml:"ages"`
	Offer    string            `yaml:"offer"`
	DailyUSD float64           `yaml:"daily_usd"`
	Levels   map[string]string `yaml:"levels"`
	CheckIns StarterCheckIns   `yaml:"check_ins"`
}

// starterBuilt is when every starter package says it was made, so the
// same source always makes the same file.
var starterBuilt = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

var (
	startersOnce sync.Once
	starters     []*StarterSprig
	startersErr  error
)

// StarterSprigs returns the bundled starter sprigs, sorted by id.
func StarterSprigs() ([]*StarterSprig, error) {
	startersOnce.Do(func() { starters, startersErr = loadStarterSprigs(starterSprigFS, "startersprigs") })
	return starters, startersErr
}

func loadStarterSprigs(fsys fs.FS, root string) ([]*StarterSprig, error) {
	entries, err := fs.ReadDir(fsys, root)
	if err != nil {
		return nil, err
	}
	var out []*StarterSprig
	seen := map[string]string{}
	for _, d := range entries {
		if !d.IsDir() {
			continue
		}
		s, err := buildStarter(fsys, path.Join(root, d.Name()), d.Name())
		if err != nil {
			return nil, fmt.Errorf("starter sprig %s: %w", d.Name(), err)
		}
		if other, dup := seen[s.Char.ID]; dup {
			return nil, fmt.Errorf("starter sprigs %s and %s both use the id %q", other, s.ID, s.Char.ID)
		}
		seen[s.Char.ID] = s.ID
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

var levelNames = []string{store.LevelRead, store.LevelDraft, store.LevelAsk, store.LevelAct}

// buildStarter assembles one starter's folder into a package and reads it
// back, so it's checked exactly like a package someone sent.
func buildStarter(fsys fs.FS, dir, id string) (*StarterSprig, error) {
	read := func(name string) ([]byte, error) { return fs.ReadFile(fsys, path.Join(dir, name)) }
	cb, err := read("character.md")
	if err != nil {
		return nil, err
	}
	c, err := character.Parse(cb)
	if err != nil {
		return nil, fmt.Errorf("character.md: %w", err)
	}
	mb, err := read("starter.yaml")
	if err != nil {
		return nil, err
	}
	var meta starterMeta
	dec := yaml.NewDecoder(bytes.NewReader(mb))
	dec.KnownFields(true)
	if err := dec.Decode(&meta); err != nil {
		return nil, fmt.Errorf("starter.yaml: %w", err)
	}
	if err := meta.check(); err != nil {
		return nil, fmt.Errorf("starter.yaml: %w", err)
	}
	files := map[string][]byte{}
	if b, err := read("avatar.json"); err == nil {
		var look avatar.Params
		if err := json.Unmarshal(b, &look); err != nil {
			return nil, fmt.Errorf("avatar.json: %w", err)
		}
		files["avatar.json"] = b
	}
	tests, _ := fs.Glob(fsys, path.Join(dir, "voice_tests", "*.yaml"))
	for _, f := range tests {
		b, err := fs.ReadFile(fsys, f)
		if err != nil {
			return nil, err
		}
		files["voice_tests/"+path.Base(f)] = b
	}
	var skills []sprigpkg.Skill
	skillFiles, _ := fs.Glob(fsys, path.Join(dir, "skills", "*.skill.md"))
	for _, f := range skillFiles {
		b, err := fs.ReadFile(fsys, f)
		if err != nil {
			return nil, err
		}
		k, err := ParseSkill(b)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path.Base(f), err)
		}
		skills = append(skills, sprigpkg.Skill{ID: k.ID, Name: k.Name, Description: k.Description, Body: k.Body})
	}
	var routines []sprigpkg.Routine
	if b, err := read("routines.json"); err == nil {
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&routines); err != nil {
			return nil, fmt.Errorf("routines.json: %w", err)
		}
	}
	if slices.Contains(meta.Tiers, character.TierKid) && len(routines) > 0 {
		return nil, errors.New("a kid starter can't have routines: kid sprigs never run anything on their own")
	}
	if hasPresets(c) {
		// Check-ins message people unprompted: they're suggested in
		// starter.yaml and stay off until the person says yes.
		return nil, errors.New("character.md turns on check-in presets: suggest them in starter.yaml check_ins instead, so they start off")
	}
	p := &sprigpkg.Package{
		Manifest: sprigpkg.Manifest{Kind: sprigpkg.Kind, Format: 1, ID: c.ID, Name: c.DisplayName(), ThicketVersion: "starter", CreatedAt: starterBuilt},
		Char:     c, Files: files, Skills: skills, Routines: routines,
	}
	raw, err := p.Bytes()
	if err != nil {
		return nil, err
	}
	back, err := sprigpkg.Read(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("the assembled package doesn't read back: %w", err)
	}
	s := &StarterSprig{ID: id, Summary: meta.Summary, Tiers: meta.Tiers, Ages: meta.Ages, Offer: meta.Offer,
		DailyUSD: meta.DailyUSD, Levels: meta.Levels, Char: back.Char, Package: raw, Routines: back.Routines, CheckIns: meta.CheckIns}
	for _, k := range back.Skills {
		s.Skills = append(s.Skills, store.Skill{ID: k.ID, Name: k.Name, Description: k.Description, Body: k.Body})
	}
	names := make([]string, 0, len(back.Files))
	for rel := range back.Files {
		if strings.HasPrefix(rel, "voice_tests/") {
			names = append(names, rel)
		}
	}
	sort.Strings(names)
	for _, rel := range names {
		var vt character.VoiceTest
		if err := yaml.Unmarshal(back.Files[rel], &vt); err != nil {
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
		vt.Prompt, vt.Reference = strings.TrimSpace(vt.Prompt), strings.TrimSpace(vt.Reference)
		vt.Name = strings.TrimSuffix(path.Base(rel), ".yaml")
		s.VoiceTests = append(s.VoiceTests, vt)
	}
	return s, nil
}

func (m *starterMeta) check() error {
	var probs []string
	if strings.TrimSpace(m.Summary) == "" || strings.TrimSpace(m.Offer) == "" {
		probs = append(probs, "summary and offer are required")
	}
	if len(m.Tiers) == 0 {
		probs = append(probs, "tiers is required")
	}
	for _, t := range m.Tiers {
		if t != character.TierSolo && t != character.TierFamily && t != character.TierKid {
			probs = append(probs, fmt.Sprintf("tier %q must be solo, family or kid", t))
		}
	}
	if slices.Contains(m.Tiers, character.TierKid) && len(m.Tiers) > 1 {
		probs = append(probs, "a kid starter is only ever a kid sprig")
	}
	if slices.Contains(m.Tiers, character.TierKid) && m.CheckIns.Any() {
		probs = append(probs, "a kid starter can't suggest check-ins: kid sprigs never message anyone unprompted")
	}
	probs = append(probs, m.CheckIns.check()...)
	if m.DailyUSD <= 0 || m.DailyUSD > 10 {
		probs = append(probs, "daily_usd must be a spend cap between 0 and 10 dollars")
	}
	for tool, lv := range m.Levels {
		if !slices.Contains(levelNames, lv) {
			probs = append(probs, fmt.Sprintf("level %q for %s must be read, draft, ask or act", lv, tool))
		}
	}
	if len(probs) > 0 {
		return errors.New(strings.Join(probs, "; "))
	}
	return nil
}

// FindStarterSprig finds a starter by its id, its sprig's id or its name.
func FindStarterSprig(name string) (*StarterSprig, error) {
	all, err := StarterSprigs()
	if err != nil {
		return nil, err
	}
	name = strings.ToLower(strings.TrimSpace(name))
	for _, s := range all {
		if name == s.ID || name == s.Char.ID || slices.Contains(s.Char.Names(), name) {
			return s, nil
		}
	}
	ids := make([]string, len(all))
	for i, s := range all {
		ids[i] = s.ID
	}
	return nil, fmt.Errorf("no starter sprig %q; the starters are %s", name, strings.Join(ids, ", "))
}

// TierLabel describes the tiers a starter may be added as.
func (s *StarterSprig) TierLabel() string {
	switch {
	case slices.Equal(s.Tiers, []string{character.TierKid}):
		label := "kid sprig"
		if s.Ages != "" {
			label += ", ages " + s.Ages
		}
		return label
	case len(s.Tiers) == 1:
		return s.Tiers[0] + " sprig"
	}
	return strings.Join(s.Tiers, " or ") + " sprig"
}

// kidSafetyNote ends every kid sprig's description (CreateKidSprig and kid
// starters alike).
const kidSafetyNote = "You are talking with a child. Keep things age-appropriate and kind. " +
	"Never ask for or encourage sharing personal details (address, school, phone, photos), never suggest meeting anyone, " +
	"and if they seem upset, unsafe or talk about hurting themselves, be gentle and encourage them to talk to a grown-up they trust."

// StarterOptions say how a starter is added.
type StarterOptions struct {
	// Tier is solo, family or kid; empty is the starter's default.
	Tier string `json:"tier,omitempty"`
	// Owner runs its routines; a solo sprig is theirs.
	Owner int64 `json:"owner,omitempty"`
	// Kids it talks to and Parents who set it up (kid sprigs).
	Kids    []int64 `json:"kids,omitempty"`
	Parents []int64 `json:"parents,omitempty"`
	// ID and Name add it under another id or name.
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
	// CheckIns turns on its suggested check-in presets; otherwise they
	// stay off.
	CheckIns bool `json:"check_ins,omitempty"`
	// CanWriteSkills: an owner or admin, who may add shared skills.
	CanWriteSkills bool `json:"-"`
}

// StarterSetup is a starter ready to install: shown first, then installed
// exactly as shown.
type StarterSetup struct {
	Starter *StarterSprig
	Pkg     *sprigpkg.Package
	Plans   []sprigpkg.SkillPlan
	Opts    StarterOptions
	Tier    string
	// KidNames and ParentNames are for the preview.
	KidNames, ParentNames []string
}

// PrepareStarter checks that a starter can be added with these options
// and plans its skills. Nothing is written. nameTaken reports a sprig
// id or name in use (besides files in charsDir).
func PrepareStarter(ctx context.Context, db *store.Store, charsDir string, nameTaken func(string) bool, s *StarterSprig, o StarterOptions) (*StarterSetup, error) {
	tier := o.Tier
	if tier == "" {
		tier = s.Tiers[0]
	}
	if !slices.Contains(s.Tiers, tier) {
		return nil, fmt.Errorf("%s can be added as a %s, not a %s sprig", s.ID, strings.TrimSuffix(s.TierLabel(), " sprig"), tier)
	}
	p, err := sprigpkg.Read(bytes.NewReader(s.Package))
	if err != nil {
		return nil, err
	}
	c := p.Char
	if id := strings.ToLower(strings.TrimSpace(o.ID)); id != "" {
		c.ID = id
	}
	if name := strings.TrimSpace(o.Name); name != "" {
		c.Name, c.Nickname = name, ""
	}
	c.CreatedBy = "starter"
	c.Avatar = "" // its look is avatar.json
	st := &StarterSetup{Starter: s, Pkg: p, Opts: o, Tier: tier}
	user := func(id int64) (*store.User, error) {
		u, err := db.UserByID(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("there's no user %d", id)
		}
		return u, err
	}
	switch tier {
	case character.TierKid:
		if len(o.Kids) == 0 || len(o.Parents) == 0 {
			return nil, errors.New("a kid sprig needs the kids it talks to and the parents who set it up")
		}
		for _, id := range o.Kids {
			u, err := user(id)
			if err != nil {
				return nil, err
			}
			if !u.IsKid() {
				return nil, fmt.Errorf("%s isn't one of the household's kids (make them a kid first)", userName(u))
			}
			st.KidNames = append(st.KidNames, userName(u))
		}
		for _, id := range o.Parents {
			u, err := user(id)
			if err != nil {
				return nil, err
			}
			if u.IsKid() {
				return nil, fmt.Errorf("%s is a kid, so can't be a parent of a kid sprig", userName(u))
			}
			st.ParentNames = append(st.ParentNames, userName(u))
		}
		if len(p.Routines) > 0 {
			return nil, errors.New("kid sprigs never run routines")
		}
		c.Household, c.Kids, c.Parents = character.TierKid, slices.Clone(o.Kids), slices.Clone(o.Parents)
		c.Initiative = character.Initiative{}
		c.Allow = nil
		if !strings.Contains(c.Body, kidSafetyNote) {
			c.Body = strings.TrimSpace(c.Body + "\n\n" + kidSafetyNote)
		}
	case character.TierSolo:
		if o.Owner == 0 {
			return nil, errors.New("a solo sprig needs the person it belongs to")
		}
		u, err := user(o.Owner)
		if err != nil {
			return nil, err
		}
		if u.IsKid() {
			return nil, errors.New("a solo sprig belongs to an adult; kids get kid sprigs")
		}
		c.BelongsTo, c.Household = o.Owner, ""
	default:
		c.BelongsTo, c.Household = 0, ""
	}
	if o.CheckIns {
		switch {
		case tier == character.TierKid:
			return nil, errors.New("kid sprigs never message anyone unprompted, so they have no check-ins")
		case !s.CheckIns.Any():
			return nil, fmt.Errorf("%s doesn't suggest any check-ins", s.ID)
		}
		c.Initiative.MorningBrief = s.CheckIns.MorningBrief
		c.Initiative.EventReminders = s.CheckIns.EventReminders
		c.Initiative.DueReminders = s.CheckIns.DueReminders
	}
	if probs := c.Validate(); len(probs) > 0 {
		return nil, errors.New(strings.Join(probs, "; "))
	}
	if len(p.Routines) > 0 && o.Owner == 0 {
		return nil, errors.New("there's no one to run its routines for yet; finish setup first")
	}
	if err := sprigpkg.CheckClash(c, charsDir, nameTaken); err != nil {
		return nil, err
	}
	if st.Plans, err = p.PlanSkills(ctx, db, ValidateSkill); err != nil {
		return nil, err
	}
	return st, nil
}

func userName(u *store.User) string {
	if u.Name != "" {
		return u.Name
	}
	return fmt.Sprintf("user %d", u.ID)
}

// levelLines are the levels a starter gets, for people to read.
func (st *StarterSetup) levelLines() string {
	if st.Tier == character.TierKid {
		return "It's read only, always: it can't change anything, schedule anything or start a conversation."
	}
	base := store.LevelDraft
	var extra []string
	tools := make([]string, 0, len(st.Starter.Levels))
	for tool := range st.Starter.Levels {
		tools = append(tools, tool)
	}
	sort.Strings(tools)
	for _, tool := range tools {
		if tool == store.AnyTool {
			base = st.Starter.Levels[tool]
			continue
		}
		extra = append(extra, tool+" at "+st.Starter.Levels[tool])
	}
	out := "Every tool it has starts at **" + base + "**"
	if base == store.LevelRead {
		out += " (it can look, never change anything, and any integration it's given later starts read only too)"
	}
	if len(extra) > 0 {
		out += ", except " + strings.Join(extra, ", ")
	}
	return out + "."
}

// Preview is everything the person reads before a starter is added: every
// file it gets, its skills, its paused routines, who it's for, its levels
// and spend cap. For a kid sprig this is the parent's preview.
func (st *StarterSetup) Preview(canWriteSkills bool) (string, error) {
	c := st.Pkg.Char
	md, err := character.Marshal(c)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("```diff\n")
	b.WriteString(sprigpkg.NewFileDiff(c.ID+".md", string(md)))
	rels := make([]string, 0, len(st.Pkg.Files))
	for rel := range st.Pkg.Files {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		b.WriteString(sprigpkg.NewFileDiff(c.ID+"/"+rel, string(st.Pkg.Files[rel])))
	}
	b.WriteString("```\n")
	for _, k := range st.Plans {
		switch {
		case k.Action == "same":
			fmt.Fprintf(&b, "Skill %s: already here, unchanged.\n", k.ID)
		case !canWriteSkills:
			fmt.Fprintf(&b, "Skill %s (%s): %s.\n", k.ID, k.Action, sprigpkg.SkillsNeedAdmin)
		case k.Action == "new":
			fmt.Fprintf(&b, "Skill %s (new): %s\n```\n%s\n```\n", k.ID, k.Description, k.Body)
		default:
			fmt.Fprintf(&b, "Skill %s (a new version after %d; the current one is kept in its history): %s\n```\n%s\n```\n", k.ID, k.Base, k.Description, k.Body)
		}
	}
	if len(st.Pkg.Routines) > 0 {
		b.WriteString("Suggested routines, **paused** until you turn them on (they'd run in your timezone):\n")
		for _, r := range st.Pkg.Routines {
			fmt.Fprintf(&b, "- %s `%s`: %s\n", r.Name, r.Schedule, r.Task)
		}
	}
	switch st.Tier {
	case character.TierKid:
		fmt.Fprintf(&b, "For a kid: %s talks only to %s, and no adult can chat with it. %s can read every chat and get a safety alert if something worrying comes up. "+
			"It sees no family memory or calendar and can't use the web unless a parent allows it later, one thing at a time.\n",
			c.DisplayName(), strings.Join(st.KidNames, " and "), strings.Join(st.ParentNames, " and "))
	case character.TierSolo:
		fmt.Fprintf(&b, "It's yours alone: nobody else in the household can talk to it or read what it knows.\n")
	default:
		fmt.Fprintf(&b, "It's a family sprig: every adult in the household can talk to it, and it shares family memory.\n")
	}
	if k := st.Starter.CheckIns; st.Tier != character.TierKid && k.Any() {
		who := "you"
		if st.Tier == character.TierFamily {
			who = "every adult it's for"
		}
		if st.Opts.CheckIns {
			fmt.Fprintf(&b, "Check-ins, **on**: %s, sent to %s like any message it starts (quiet hours and its daily limit apply).\n", k, who)
		} else {
			fmt.Fprintf(&b, "Suggested check-ins, **off** until you turn them on: %s.\n", k)
		}
	}
	b.WriteString(st.levelLines() + "\n")
	fmt.Fprintf(&b, "Spend cap: $%.2f a day for %s. No memories, chats or secrets come with it.\n", st.Starter.DailyUSD, c.DisplayName())
	return b.String(), nil
}

// StarterInstalled is what InstallStarter did.
type StarterInstalled struct {
	Path     string
	Routines []int64  // created paused
	Skills   []string // as sprigpkg.Installed.Skills
}

// InstallStarter adds a prepared starter: the package is installed (every
// tool at draft, its skills), then the starter's levels and spend cap are
// set and its routines are created paused.
func InstallStarter(ctx context.Context, db *store.Store, charsDir string, st *StarterSetup) (*StarterInstalled, error) {
	routines := st.Pkg.Routines
	st.Pkg.Routines = nil // created paused below, not running
	var jobs []int64
	res, err := st.Pkg.Install(ctx, db, charsDir, sprigpkg.InstallOptions{Owner: st.Opts.Owner, Plans: st.Plans, CanWriteSkills: st.Opts.CanWriteSkills,
		// Levels, paused routines and the cap go in before the sprig is
		// live, all or nothing: a bill tracker without its read level
		// isn't installed at all.
		BeforeLive: func(ctx context.Context) (func(context.Context), error) {
			var err error
			jobs, err = applyStarterExtras(ctx, db, st, routines)
			if err != nil {
				return nil, err
			}
			return func(bg context.Context) { removeStarterExtras(bg, db, st, jobs) }, nil
		}})
	st.Pkg.Routines = routines
	if err != nil {
		return nil, err
	}
	return &StarterInstalled{Path: res.Path, Skills: res.Skills, Routines: jobs}, nil
}

// applyStarterExtras sets a starter's levels, paused routines and spend
// cap. On failure it removes what it did and returns the error.
func applyStarterExtras(ctx context.Context, db *store.Store, st *StarterSetup, routines []sprigpkg.Routine) (jobs []int64, err error) {
	c := st.Pkg.Char
	defer func() {
		if err != nil {
			removeStarterExtras(context.WithoutCancel(ctx), db, st, jobs)
			jobs = nil
		}
	}()
	if st.Tier != character.TierKid {
		tools := make([]string, 0, len(st.Starter.Levels))
		for tool := range st.Starter.Levels {
			tools = append(tools, tool)
		}
		sort.Strings(tools)
		for _, tool := range tools {
			if err := db.SetToolLevel(ctx, c.ID, tool, st.Starter.Levels[tool]); err != nil {
				return jobs, fmt.Errorf("the level for %s: %w", tool, err)
			}
		}
	}
	for _, r := range routines {
		j, err := db.CreateJob(ctx, store.Job{UserID: st.Opts.Owner, Character: c.ID, Name: r.Name, Schedule: r.Schedule, Prompt: r.Task, Kind: store.JobCron})
		if j != nil && j.ID != 0 {
			jobs = append(jobs, j.ID)
		}
		if err == nil {
			err = db.SetJobEnabled(ctx, j.ID, false)
		}
		if err != nil {
			return jobs, fmt.Errorf("the routine %q: %w", r.Name, err)
		}
	}
	if err := db.SetSetting(ctx, starterCapKey(c.ID), strconv.FormatFloat(st.Starter.DailyUSD, 'f', -1, 64)); err != nil {
		return jobs, fmt.Errorf("the spend cap: %w", err)
	}
	return jobs, nil
}

// removeStarterExtras undoes applyStarterExtras. The package install has
// already cleared any levels an earlier sprig with this id left, so the
// starter's levels are simply removed.
func removeStarterExtras(ctx context.Context, db *store.Store, st *StarterSetup, jobs []int64) {
	c := st.Pkg.Char
	for _, id := range jobs {
		_ = db.DeleteJob(ctx, id)
	}
	for tool := range st.Starter.Levels {
		_ = db.SetToolLevel(ctx, c.ID, tool, "")
	}
	_ = db.DeleteSetting(ctx, starterCapKey(c.ID))
}

func starterCapKey(id string) string { return "budget.character." + id + ".daily_usd" }

// --- in chat ---

// starterApproval is what the person approves in chat.
type starterApproval struct {
	Starter string               `json:"starter"`
	Opts    StarterOptions       `json:"opts"`
	Plans   []sprigpkg.SkillPlan `json:"plans,omitempty"`
}

// sprigNameTaken reports a sprig id or name already in use.
func (e *Engine) sprigNameTaken(s string) bool {
	if _, ok := e.Chars.Get(strings.ToLower(s)); ok {
		return true
	}
	_, ok := e.Chars.FindByName(s)
	return ok
}

func (e *Engine) toolStarterSprigs(ctx context.Context, t *turn, args json.RawMessage) (string, error) {
	a, err := decode[struct {
		Action   string   `json:"action"`
		Starter  string   `json:"starter"`
		Tier     string   `json:"tier"`
		Kids     []string `json:"kids"`
		ID       string   `json:"id"`
		Name     string   `json:"name"`
		CheckIns bool     `json:"check_ins"`
	}](args)
	if err != nil {
		return "", err
	}
	if a.Action != "add" {
		return e.listStarters()
	}
	if t.user.IsKid() {
		return "", errors.New("only a grown-up can add sprigs")
	}
	s, err := FindStarterSprig(a.Starter)
	if err != nil {
		return "", err
	}
	o := StarterOptions{Tier: strings.ToLower(strings.TrimSpace(a.Tier)), Owner: t.user.ID, ID: a.ID, Name: a.Name, CheckIns: a.CheckIns}
	if o.Tier == "" {
		o.Tier = s.Tiers[0]
	}
	if err := mayStartCheckIns(t.user, o); err != nil {
		return "", err
	}
	switch o.Tier {
	case character.TierKid:
		if !t.user.IsAdmin() {
			return "", errors.New("only the owner or an admin can set up a kid sprig: ask them to")
		}
		if o.Kids, err = e.resolveKids(ctx, a.Kids); err != nil {
			return "", err
		}
		o.Parents = []int64{t.user.ID}
	case character.TierSolo:
		if !t.isDM {
			return "A sprig of their own is set up in a DM, never in a group channel: ask them to continue in a DM (or the web app) and add it there.", nil
		}
	}
	st, err := PrepareStarter(ctx, e.DB, e.Chars.Dir(), e.sprigNameTaken, s, o)
	if err != nil {
		var c *sprigpkg.Clash
		if errors.As(err, &c) {
			return "", fmt.Errorf("%w: ask the user what %s to give it (suggest %q) and call this again with that %s", err, c.What, c.Suggest, c.What)
		}
		return "", fmt.Errorf("%s can't be added: %w", s.ID, err)
	}
	preview, err := st.Preview(t.user.IsAdmin())
	if err != nil {
		return "", err
	}
	b, _ := json.Marshal(starterApproval{Starter: s.ID, Opts: o, Plans: st.Plans})
	name := st.Pkg.Char.DisplayName()
	intro := fmt.Sprintf("🌱 Here's everything %s, the %s starter, will get. Nothing is added until you say so.\n", name, s.ID)
	if o.Tier == character.TierKid {
		intro = fmt.Sprintf("🌱 Parent preview: here's everything %s, the %s starter, will be and say to %s. Nothing is added until you say so.\n", name, s.ID, strings.Join(st.KidNames, " and "))
	}
	t.askAs(t.char, intro+preview+"Reply **approve** or **deny**.",
		&store.Approval{UserID: t.user.ID, Character: t.char.ID, ChatRef: t.chatRef, Tool: internalPrefix + "add_starter", Args: string(b)})
	if o.Tier != character.TierSolo {
		e.logActivity(ctx, "info", "character", t.char.ID, t.user.ID, "proposed starter sprig "+name, "")
	}
	return fmt.Sprintf("PENDING: the user is being shown every file %s will get, its levels and spend cap, and asked to approve. Tell them briefly to look it over; don't say it's added.", name), nil
}

// listStarters is the starter_sprigs list.
func (e *Engine) listStarters() (string, error) {
	all, err := StarterSprigs()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("Starter sprigs (add one with action add):\n")
	for _, s := range all {
		fmt.Fprintf(&b, "- %s: %s (%s). %s Offer it %s.", s.ID, s.Char.DisplayName(), s.TierLabel(), s.Summary, s.Offer)
		if s.CheckIns.Any() {
			fmt.Fprintf(&b, " Suggested check-ins (off unless they want them): %s.", s.CheckIns)
		}
		if _, ok := e.Chars.Get(s.Char.ID); ok {
			fmt.Fprintf(&b, " A sprig with the id %q is already here: add it only if they want another (with a different id and name).", s.Char.ID)
		}
		b.WriteString("\n")
	}
	b.WriteString("Kid starters need the kid invited as a kid first, and only the owner or an admin can add one: they become its parent and see it first (the parent preview). Routines arrive paused; offer to turn them on once they've tried it. Ask whether they'd like a starter's suggested check-ins before adding it (check_ins: true); a shared sprig's check-ins need an admin.\n")
	return b.String(), nil
}

// mayStartCheckIns: like set_initiative, a shared sprig's check-ins (which
// message every adult) are an admin's to turn on; a sprig of your own is
// yours.
func mayStartCheckIns(u *store.User, o StarterOptions) error {
	if o.CheckIns && o.Tier == character.TierFamily && !u.IsAdmin() {
		return errors.New("a family sprig's check-ins message every adult, so only an admin can turn them on: add it without check_ins, and an admin can turn them on later")
	}
	return nil
}

// resolveKids turns names or ids into the household's kids. With none
// named and exactly one kid, it's them.
func (e *Engine) resolveKids(ctx context.Context, names []string) ([]int64, error) {
	users, err := e.DB.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	var kids []store.User
	for _, u := range users {
		if u.IsKid() {
			kids = append(kids, u)
		}
	}
	if len(kids) == 0 {
		return nil, errors.New("there are no kids in the household yet: invite the kid first (invite_person with role kid), then add it for them")
	}
	list := func() string {
		var ns []string
		for _, k := range kids {
			ns = append(ns, fmt.Sprintf("%s (%d)", userName(&k), k.ID))
		}
		return strings.Join(ns, ", ")
	}
	if len(names) == 0 {
		if len(kids) == 1 {
			return []int64{kids[0].ID}, nil
		}
		return nil, fmt.Errorf("which kids is it for? The household's kids are %s", list())
	}
	var out []int64
	for _, n := range names {
		n = strings.TrimSpace(n)
		found := false
		for _, k := range kids {
			if strings.EqualFold(k.Name, n) || strconv.FormatInt(k.ID, 10) == n {
				if !slices.Contains(out, k.ID) {
					out = append(out, k.ID)
				}
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("%q isn't one of the household's kids (they are %s); a kid must be invited as a kid first", n, list())
		}
	}
	return out, nil
}

// addStarter adds an approved starter, exactly as shown.
func (e *Engine) addStarter(ctx context.Context, user *store.User, args json.RawMessage) (string, error) {
	var a starterApproval
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	s, err := FindStarterSprig(a.Starter)
	if err != nil {
		return "", err
	}
	o := a.Opts
	o.Owner, o.CanWriteSkills = user.ID, user.IsAdmin()
	if err := mayStartCheckIns(user, o); err != nil {
		return "", err
	}
	if o.Tier == character.TierKid {
		if !user.IsAdmin() {
			return "", errors.New("only the owner or an admin can set up a kid sprig")
		}
		if !slices.Contains(o.Parents, user.ID) {
			return "", errors.New("the parent approving a kid sprig must be one of its parents")
		}
	}
	st, err := PrepareStarter(ctx, e.DB, e.Chars.Dir(), e.sprigNameTaken, s, o)
	if err != nil {
		return "", err
	}
	st.Plans = a.Plans // install only what was shown: a changed library stops it
	res, err := InstallStarter(ctx, e.DB, e.Chars.Dir(), st)
	if err != nil {
		return "", err
	}
	if err := e.Chars.Load(); err != nil {
		e.Log.Warn("reload sprigs after adding a starter", "err", err)
	}
	c := st.Pkg.Char
	e.trailLevel(ctx, user, c.ID, store.AnyTool, "", store.LevelDraft, "a starter sprig starts at draft")
	if st.Tier == character.TierKid {
		e.addTrail(ctx, user.ID, c.ID, store.TrailGrant, fmt.Sprintf("kid sprig %s added from the %s starter for %s, parents %s", c.DisplayName(), s.ID,
			strings.Join(st.KidNames, ", "), strings.Join(st.ParentNames, ", ")), "", nil)
	}
	if st.Tier != character.TierSolo {
		e.logActivity(ctx, "info", "character", c.ID, user.ID, fmt.Sprintf("added starter sprig %s (%s)", c.DisplayName(), s.ID), res.Path)
	}
	out := fmt.Sprintf("%s has been added from the %s starter (%s). %s", c.DisplayName(), s.ID, res.Path, st.levelLines())
	if st.Tier == character.TierKid {
		out = fmt.Sprintf("%s has been added for %s from the %s starter (%s). Only they can talk to it; %s can read its chats.", c.DisplayName(),
			strings.Join(st.KidNames, " and "), s.ID, res.Path, strings.Join(st.ParentNames, " and "))
	}
	if len(res.Routines) > 0 {
		ids := make([]string, len(res.Routines))
		for i, id := range res.Routines {
			ids[i] = fmt.Sprintf("#%d", id)
		}
		out += fmt.Sprintf(" Its suggested routines are paused (jobs %s): offer to turn them on (update_job resume) once they've tried it.", strings.Join(ids, ", "))
	}
	if k := s.CheckIns; st.Tier != character.TierKid && k.Any() {
		if o.CheckIns {
			out += " Its check-ins are on: " + k.String() + "."
		} else {
			out += fmt.Sprintf(" Its suggested check-ins are off (%s): offer them once (set_initiative with sprig %q).", k, c.ID)
		}
	}
	if len(res.Skills) > 0 {
		out += " Skills: " + strings.Join(res.Skills, ", ") + "."
	}
	return out, nil
}

// starterGuide tells the host about the starters, once per process. It's
// in every host prompt, so it stays short: the details (who each is for,
// when to offer it) come from starter_sprigs list.
var starterGuide = sync.OnceValue(func() string {
	all, err := StarterSprigs()
	if err != nil || len(all) == 0 {
		return ""
	}
	names := make([]string, len(all))
	for i, s := range all {
		names[i] = fmt.Sprintf("%s (%s)", s.ID, s.TierLabel())
	}
	return "\n## Starter sprigs\nReady-made sprigs: " + strings.Join(names, ", ") + `. Before interviewing for a new sprig, call starter_sprigs list to see if one fits and when to offer it; offer it once, in the user's own terms ("You said you have a 9-year-old: want a reading buddy?"), and never push. Adding one (action add) shows the user everything and asks them to approve.
`
})
