package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/avatar"
	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/store"
)

func TestStarterSprigsAreComplete(t *testing.T) {
	all, err := StarterSprigs()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"bill-tracker", "chore-chart", "homework-helper", "meal-planner", "reading-buddy"}
	var got []string
	for _, s := range all {
		got = append(got, s.ID)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("starters %v, want %v", got, want)
	}
	for _, s := range all {
		c := s.Char
		if probs := c.Validate(); len(probs) > 0 {
			t.Errorf("%s: %v", s.ID, probs)
		}
		if n := len(c.Personality.Traits); n < 3 || n > 4 {
			t.Errorf("%s: %d traits; the authoring guide says 3-4", s.ID, n)
		}
		if len(c.Voice.Style) < 3 || len(c.Voice.NeverSay) < 2 || !strings.Contains(c.Body, "Example lines") {
			t.Errorf("%s: a real voice needs style rules, never-say phrases and example lines", s.ID)
		}
		if len(s.VoiceTests) < 5 {
			t.Errorf("%s: %d voice tests, want at least 5", s.ID, len(s.VoiceTests))
		}
		if len(s.Skills) == 0 {
			t.Errorf("%s: no skills", s.ID)
		}
		for _, k := range s.Skills {
			if h := SkillHints(k); len(h) > 0 {
				t.Errorf("%s: skill %s: %v", s.ID, k.ID, h)
			}
			for _, part := range []string{"Needs:", "Done when:", "Watch out:"} {
				if !strings.Contains(k.Body, part) {
					t.Errorf("%s: skill %s has no %q section", s.ID, k.ID, part)
				}
			}
		}
		var look avatar.Params
		if b := s.packageFile(t, "avatar.json"); json.Unmarshal(b, &look) != nil || look.Validate() != nil {
			t.Errorf("%s: avatar.json is missing or invalid", s.ID)
		}
		if s.DailyUSD <= 0 {
			t.Errorf("%s: no spend cap", s.ID)
		}
		kid := slices.Contains(s.Tiers, character.TierKid)
		if kid {
			if len(c.Tools) > 0 || len(s.Routines) > 0 || c.Initiative.CheckIns || c.Initiative.FollowUps {
				t.Errorf("%s: a kid starter starts with no tools, routines or initiative", s.ID)
			}
			continue
		}
		if len(s.Routines) == 0 {
			t.Errorf("%s: no suggested routines", s.ID)
		}
		if !c.HasScope(character.ScopeSchedule) {
			t.Errorf("%s: has routines but no schedule scope", s.ID)
		}
		for _, r := range s.Routines {
			used := false
			for _, k := range s.Skills {
				used = used || strings.Contains(r.Task, k.ID)
			}
			if !used || !strings.Contains(strings.ToLower(r.Task), "report nothing") {
				t.Errorf("%s: routine %q should name its skill and say when to report nothing", s.ID, r.Name)
			}
		}
		if c.Initiative.QuietHours == "" || c.Initiative.DailyMessages == 0 {
			t.Errorf("%s: set quiet hours and a daily budget", s.ID)
		}
	}
}

// packageFile reads one file from the starter's source folder.
func (s *StarterSprig) packageFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := starterSprigFS.ReadFile("startersprigs/" + s.ID + "/" + name)
	if err != nil {
		return nil
	}
	return b
}

// TestStarterVoiceTests checks each starter's voice tests as far as can be
// done without a model: every reference passes the never-say check done
// in code, uses catchphrases sparingly, stays short and in role.
func TestStarterVoiceTests(t *testing.T) {
	all, err := StarterSprigs()
	if err != nil {
		t.Fatal(err)
	}
	offRole := regexp.MustCompile(`(?i)\b(language model|as an ai|i'?m an ai|assistant|chatbot)\b`)
	for _, s := range all {
		c := s.Char
		for _, vt := range s.VoiceTests {
			name := s.ID + "/" + vt.Name
			if vt.Prompt == "" || vt.Reference == "" || strings.TrimSpace(vt.Notes) == "" {
				t.Errorf("%s: needs a prompt, a reference and notes", name)
				continue
			}
			if banned := bannedPhrases(c, vt.Reference); len(banned) > 0 {
				t.Errorf("%s: %v", name, banned)
			}
			if offRole.MatchString(vt.Reference) {
				t.Errorf("%s: the reference steps out of role", name)
			}
			if n := len(strings.Fields(vt.Reference)); n > 60 {
				t.Errorf("%s: %d words; these sprigs talk short", name, n)
			}
			used := 0
			for _, p := range c.Personality.Catchphrases {
				used += strings.Count(strings.ToLower(vt.Reference), strings.ToLower(strings.TrimSuffix(p, ".")))
			}
			if used > 1 {
				t.Errorf("%s: %d catchphrases in one reply", name, used)
			}
		}
	}
	// The homework helper never gives the answer, even in its own tests.
	sage, _ := FindStarterSprig("homework-helper")
	for _, vt := range sage.VoiceTests {
		if vt.Name == "02-just-tell-me" && strings.Contains(vt.Reference, "56") {
			t.Fatal("Sage gave the answer")
		}
	}
}

// starterFixture is a fresh install with an owner (admin), another adult
// and a kid.
func starterFixture(t *testing.T) (*taintFixture, *store.User, *store.User) {
	f := newTaintFixture(t)
	ctx := context.Background()
	kim, err := f.e.DB.CreateUser(ctx, "web", "u2", "Kim", store.RoleMember)
	if err != nil {
		t.Fatal(err)
	}
	mia, err := f.e.DB.CreateUser(ctx, "web", "u3", "Mia", store.RoleKid)
	if err != nil {
		t.Fatal(err)
	}
	return f, kim, mia
}

func TestStarterSprigsInstallOnAFreshInstall(t *testing.T) {
	all, err := StarterSprigs()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range all {
		t.Run(s.ID, func(t *testing.T) {
			f, kim, mia := starterFixture(t)
			ctx := context.Background()
			o := StarterOptions{Owner: f.user.ID, CanWriteSkills: true}
			if s.Tiers[0] == character.TierKid {
				o.Kids, o.Parents = []int64{mia.ID}, []int64{f.user.ID}
			}
			st, err := PrepareStarter(ctx, f.e.DB, f.e.Chars.Dir(), f.e.sprigNameTaken, s, o)
			if err != nil {
				t.Fatal(err)
			}
			preview, err := st.Preview(true)
			if err != nil || !strings.Contains(preview, "+++ b/characters/"+s.Char.ID+".md") || !strings.Contains(preview, "Spend cap") {
				t.Fatalf("preview: %v\n%s", err, preview)
			}
			_, err = InstallStarter(ctx, f.e.DB, f.e.Chars.Dir(), st)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.e.Chars.Load(); err != nil {
				t.Fatal(err)
			}
			c, ok := f.e.Chars.Get(s.Char.ID)
			if !ok {
				t.Fatalf("%s not loaded: %v", s.Char.ID, f.e.Chars.Errors())
			}
			if c.CreatedBy != "starter" || c.Tier() != s.Tiers[0] {
				t.Fatalf("created_by %q, tier %q", c.CreatedBy, c.Tier())
			}
			// Its voice tests and look are where voice-check and the
			// avatar read them.
			tests, err := character.LoadVoiceTests(f.e.Chars.Dir(), c.ID)
			if err != nil || len(tests) != len(s.VoiceTests) {
				t.Fatalf("voice tests: %d, %v", len(tests), err)
			}
			for _, vt := range tests {
				if vt.Reference == "" {
					t.Fatalf("%s has no reference", vt.Name)
				}
			}
			if _, err := os.Stat(avatar.Path(f.e.Chars.Dir(), c.ID)); err != nil {
				t.Fatal("no avatar.json")
			}
			for _, k := range s.Skills {
				if _, err := f.e.DB.SkillByID(ctx, k.ID, 0); err != nil {
					t.Fatalf("skill %s: %v", k.ID, err)
				}
			}
			if v, ok, _ := f.e.DB.Setting(ctx, "budget.character."+c.ID+".daily_usd"); !ok || v == "" {
				t.Fatal("no spend cap")
			}
			jobs, _ := f.e.DB.CharacterJobs(ctx, c.ID)
			if len(jobs) != len(s.Routines) {
				t.Fatalf("%d jobs for %d routines", len(jobs), len(s.Routines))
			}
			for _, j := range jobs {
				if j.Enabled || j.UserID != f.user.ID {
					t.Fatalf("routine %q should be paused and the owner's: %+v", j.Name, j)
				}
			}
			if c.IsKid() {
				checkKidStarter(t, f, c, kim, mia)
				return
			}
			want := map[string]string{store.AnyTool: store.LevelDraft}
			for tool, lv := range s.Levels {
				want[tool] = lv
			}
			for tool, lv := range want {
				if got, _ := f.e.DB.ExactToolLevel(ctx, c.ID, tool); got != lv {
					t.Errorf("level for %s: %q, want %q", tool, got, lv)
				}
			}
			if c.UsableBy(mia) {
				t.Fatal("a kid can talk to an adult sprig")
			}
		})
	}
}

// checkKidStarter is the kid validation: the file is valid as a kid sprig,
// only its kid can talk to it, it's read only and is offered no tool that
// changes anything.
func checkKidStarter(t *testing.T, f *taintFixture, c *character.Character, kim, mia *store.User) {
	t.Helper()
	ctx := context.Background()
	if probs := c.Validate(); len(probs) > 0 {
		t.Fatalf("kid validation: %v", probs)
	}
	if !slices.Equal(c.Kids, []int64{mia.ID}) || !slices.Equal(c.Parents, []int64{f.user.ID}) || len(c.Allow) > 0 || len(c.Tools) > 0 {
		t.Fatalf("kid sprig fields: %+v", c)
	}
	if !c.UsableBy(mia) || c.UsableBy(f.user) || c.UsableBy(kim) {
		t.Fatal("only the kid may talk to a kid sprig")
	}
	if !strings.Contains(c.Body, kidSafetyNote) {
		t.Fatal("no safety note")
	}
	tr := &turn{user: mia, char: c, chatRef: "web:9"}
	for name, tool := range f.e.toolsFor(ctx, tr) {
		if changes(tool) {
			t.Errorf("a kid sprig is offered %s, which changes things", name)
		}
		if lv := f.e.toolLevel(ctx, c, tool); lv != store.LevelRead {
			t.Errorf("%s at %s", name, lv)
		}
	}
	if !c.IsParent(f.user.ID) || c.IsParent(kim.ID) {
		t.Fatal("parents")
	}
}

func TestKidStarterNeedsAKidAndAParent(t *testing.T) {
	f, kim, mia := starterFixture(t)
	ctx := context.Background()
	s, err := FindStarterSprig("poppy")
	if err != nil || s.ID != "reading-buddy" {
		t.Fatalf("find by sprig id: %v %v", s, err)
	}
	for _, o := range []StarterOptions{
		{Owner: f.user.ID, Parents: []int64{f.user.ID}},                        // no kid
		{Owner: f.user.ID, Kids: []int64{kim.ID}, Parents: []int64{f.user.ID}}, // an adult as the kid
		{Owner: f.user.ID, Kids: []int64{mia.ID}, Parents: []int64{mia.ID}},    // a kid as the parent
		{Owner: f.user.ID, Kids: []int64{mia.ID}, Parents: []int64{f.user.ID}, Tier: character.TierFamily},
	} {
		if _, err := PrepareStarter(ctx, f.e.DB, f.e.Chars.Dir(), f.e.sprigNameTaken, s, o); err == nil {
			t.Errorf("accepted %+v", o)
		}
	}
	// The bill tracker is solo by default, family on request, never kid.
	bills, _ := FindStarterSprig("bill-tracker")
	st, err := PrepareStarter(ctx, f.e.DB, f.e.Chars.Dir(), f.e.sprigNameTaken, bills, StarterOptions{Owner: kim.ID})
	if err != nil || st.Pkg.Char.BelongsTo != kim.ID {
		t.Fatalf("solo: %v", err)
	}
	if st, err = PrepareStarter(ctx, f.e.DB, f.e.Chars.Dir(), f.e.sprigNameTaken, bills, StarterOptions{Owner: kim.ID, Tier: character.TierFamily}); err != nil || st.Pkg.Char.Tier() != character.TierFamily {
		t.Fatalf("family: %v", err)
	}
	if _, err := PrepareStarter(ctx, f.e.DB, f.e.Chars.Dir(), f.e.sprigNameTaken, bills, StarterOptions{Owner: kim.ID, Tier: character.TierKid, Kids: []int64{mia.ID}, Parents: []int64{kim.ID}}); err == nil {
		t.Fatal("the bill tracker became a kid sprig")
	}
}

func TestStarterSprigsFromChat(t *testing.T) {
	f, kim, _ := starterFixture(t)
	ctx := context.Background()
	f.char = &character.Character{ID: "juniper", Name: "Juniper", Host: true, Tools: []string{"characters"}}
	tr := f.turn("what sprigs could we add?")
	out, err := f.e.toolStarterSprigs(ctx, tr, json.RawMessage(`{"action":"list"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"homework-helper", "reading-buddy", "meal-planner", "bill-tracker", "chore-chart"} {
		if !strings.Contains(out, id) {
			t.Errorf("list has no %s:\n%s", id, out)
		}
	}

	// A reading buddy for the household's only kid: the parent preview,
	// then nothing until it's approved.
	tr = f.turn("yes, a reading buddy for Mia please")
	out, err = f.e.toolStarterSprigs(ctx, tr, json.RawMessage(`{"action":"add","starter":"reading-buddy"}`))
	if err != nil || !strings.HasPrefix(out, "PENDING") {
		t.Fatalf("%v %s", err, out)
	}
	if len(tr.afterReply) != 1 || tr.afterReply[0].approval == nil {
		t.Fatalf("approval: %+v", tr.afterReply)
	}
	msg := tr.afterReply[0].text
	for _, want := range []string{"Parent preview", "+++ b/characters/poppy.md", "household: kid", "talks only to Mia", "read only", "Spend cap: $0.25"} {
		if !strings.Contains(msg, want) {
			t.Errorf("parent preview has no %q:\n%s", want, msg)
		}
	}
	if _, ok := f.e.Chars.Get("poppy"); ok {
		t.Fatal("added before approval")
	}
	if _, err := os.Stat(filepath.Join(f.e.Chars.Dir(), "poppy.md")); err == nil {
		t.Fatal("written before approval")
	}
	a := tr.afterReply[0].approval
	if a.Tool != internalPrefix+"add_starter" {
		t.Fatalf("tool %s", a.Tool)
	}
	res, err := f.e.internalAction(ctx, f.user, a)
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := f.e.Chars.Get("poppy"); !ok || !c.IsKid() || !strings.Contains(res, "Mia") {
		t.Fatalf("added: %+v %s", c, res)
	}
	// Again: taken, with a suggestion.
	if _, err := f.e.toolStarterSprigs(ctx, f.turn("another"), json.RawMessage(`{"action":"add","starter":"reading-buddy"}`)); err == nil || !strings.Contains(err.Error(), "poppy-2") {
		t.Fatalf("clash: %v", err)
	}

	// The meal planner: a family sprig, its routine paused.
	tr = f.turn("the meal planner sounds good")
	if _, err := f.e.toolStarterSprigs(ctx, tr, json.RawMessage(`{"action":"add","starter":"meal-planner"}`)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tr.afterReply[0].text, "**paused**") {
		t.Fatalf("routines not shown as paused:\n%s", tr.afterReply[0].text)
	}
	if res, err = f.e.internalAction(ctx, f.user, tr.afterReply[0].approval); err != nil || !strings.Contains(res, "paused") {
		t.Fatalf("%v %s", err, res)
	}
	jobs, _ := f.e.DB.CharacterJobs(ctx, "basil")
	if len(jobs) != 1 || jobs[0].Enabled {
		t.Fatalf("jobs %+v", jobs)
	}
	if lv, _ := f.e.DB.ExactToolLevel(ctx, "basil", "schedule_job"); lv != store.LevelAsk {
		t.Fatalf("schedule_job level %q", lv)
	}

	// Someone who isn't an admin can't set up a kid sprig, and a private
	// sprig isn't set up in a group chat.
	member := &turn{user: kim, char: f.char, chatRef: "web:2", userText: "add sage for mia"}
	if _, err := f.e.toolStarterSprigs(ctx, member, json.RawMessage(`{"action":"add","starter":"homework-helper"}`)); err == nil {
		t.Fatal("a member set up a kid sprig")
	}
	if out, err := f.e.toolStarterSprigs(ctx, member, json.RawMessage(`{"action":"add","starter":"bill-tracker"}`)); err != nil || !strings.Contains(out, "DM") {
		t.Fatalf("solo in a group: %v %s", err, out)
	}
	member.isDM = true
	if _, err := f.e.toolStarterSprigs(ctx, member, json.RawMessage(`{"action":"add","starter":"bill-tracker"}`)); err != nil {
		t.Fatal(err)
	}
	if res, err = f.e.internalAction(ctx, kim, member.afterReply[0].approval); err != nil {
		t.Fatal(err)
	}
	clover, ok := f.e.Chars.Get("clover")
	if !ok || clover.BelongsTo != kim.ID {
		t.Fatalf("clover: %+v", clover)
	}
	// A member can't add shared skills: the sprig comes without them.
	if !strings.Contains(res, "bill-tracker "+"not imported") {
		t.Fatalf("skills for a member: %s", res)
	}
	if lv, _ := f.e.DB.ExactToolLevel(ctx, "clover", store.AnyTool); lv != store.LevelRead {
		t.Fatalf("clover's level %q: it must never be able to pay", lv)
	}
}

func TestStarterGuideInHostPrompt(t *testing.T) {
	host := &character.Character{ID: "juniper", Name: "Juniper", Host: true, Tools: []string{"characters"}}
	p := buildSystemPrompt(promptInput{char: host, user: &store.User{ID: 1, Role: store.RoleOwner}, now: time.Now(), onboarded: false, hostTools: true, isDM: true})
	for _, want := range []string{"## Starter sprigs", "reading-buddy", "starter_sprigs", "9-year-old"} {
		if !strings.Contains(p, want) {
			t.Errorf("host prompt has no %q", want)
		}
	}
	other := &character.Character{ID: "basil", Name: "Basil"}
	if p := buildSystemPrompt(promptInput{char: other, user: &store.User{ID: 1, Role: store.RoleOwner}, now: time.Now(), onboarded: true}); strings.Contains(p, "## Starter sprigs") {
		t.Error("only the host offers starters")
	}
}

// A starter goes in all or nothing: if one of its levels, routines or its
// cap can't be set, the sprig isn't added and nothing is left behind.
func TestStarterInstallIsAllOrNothing(t *testing.T) {
	f, _, _ := starterFixture(t)
	ctx := context.Background()
	s, err := FindStarterSprig("bill-tracker")
	if err != nil {
		t.Fatal(err)
	}
	st, err := PrepareStarter(ctx, f.e.DB, f.e.Chars.Dir(), f.e.sprigNameTaken, s, StarterOptions{Owner: f.user.ID, CanWriteSkills: true})
	if err != nil {
		t.Fatal(err)
	}
	broken := *st.Starter
	broken.Levels = map[string]string{"*": store.LevelRead, "zz_last": "sideways"} // the second can't be set
	st.Starter = &broken
	if _, err := InstallStarter(ctx, f.e.DB, f.e.Chars.Dir(), st); err == nil {
		t.Fatal("installed with a level that couldn't be set")
	}
	id := st.Pkg.Char.ID
	if _, err := os.Stat(filepath.Join(f.e.Chars.Dir(), id+".md")); !os.IsNotExist(err) {
		t.Fatalf("the character file was left: %v", err)
	}
	if jobs, _ := f.e.DB.CharacterJobs(ctx, id); len(jobs) != 0 {
		t.Fatalf("routines left: %+v", jobs)
	}
	rows, _ := f.e.DB.ToolLevels(ctx)
	for _, r := range rows {
		if r.Character == id {
			t.Fatalf("level left: %+v", r)
		}
	}
	if _, ok, _ := f.e.DB.Setting(ctx, starterCapKey(id)); ok {
		t.Fatal("the spend cap was left")
	}
	// And it can be added cleanly afterwards.
	st, err = PrepareStarter(ctx, f.e.DB, f.e.Chars.Dir(), f.e.sprigNameTaken, s, StarterOptions{Owner: f.user.ID, CanWriteSkills: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InstallStarter(ctx, f.e.DB, f.e.Chars.Dir(), st); err != nil {
		t.Fatal(err)
	}
	if lv, _ := f.e.DB.ExactToolLevel(ctx, id, "*"); lv != store.LevelRead {
		t.Fatalf("bill tracker's level = %q, want read", lv)
	}
}

// Starters suggest check-in presets but never turn them on by themselves:
// they're on only when the person says yes as it's added, and a shared
// sprig's only when an admin does (as with set_initiative).
func TestStarterCheckInsAreOptIn(t *testing.T) {
	f, kim, mia := starterFixture(t)
	ctx := context.Background()
	f.char = &character.Character{ID: "juniper", Name: "Juniper", Host: true, Tools: []string{"characters"}}
	all, err := StarterSprigs()
	if err != nil {
		t.Fatal(err)
	}
	suggests := 0
	for _, s := range all {
		if hasPresets(s.Char) {
			t.Errorf("%s's packaged character has check-ins on", s.ID)
		}
		if s.CheckIns.Any() {
			suggests++
		}
	}
	if suggests == 0 {
		t.Fatal("no starter suggests check-ins")
	}

	// Without check_ins: shown as off, added off, offered afterwards.
	tr := f.turn("the meal planner please")
	if _, err := f.e.toolStarterSprigs(ctx, tr, json.RawMessage(`{"action":"add","starter":"meal-planner"}`)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tr.afterReply[0].text, "Suggested check-ins, **off**") {
		t.Fatalf("preview:\n%s", tr.afterReply[0].text)
	}
	res, err := f.e.internalAction(ctx, f.user, tr.afterReply[0].approval)
	if err != nil {
		t.Fatal(err)
	}
	if basil, _ := f.e.Chars.Get("basil"); basil == nil || hasPresets(basil) || !strings.Contains(res, "set_initiative") {
		t.Fatalf("basil %+v: %s", basil, res)
	}

	// With check_ins, from an admin: on, exactly as suggested.
	tr = f.turn("the chore chart, with the morning brief")
	if _, err := f.e.toolStarterSprigs(ctx, tr, json.RawMessage(`{"action":"add","starter":"chore-chart","check_ins":true}`)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tr.afterReply[0].text, "Check-ins, **on**: a morning brief at 07:30") {
		t.Fatalf("preview:\n%s", tr.afterReply[0].text)
	}
	if _, err := f.e.internalAction(ctx, f.user, tr.afterReply[0].approval); err != nil {
		t.Fatal(err)
	}
	thyme, _ := f.e.Chars.Get("thyme")
	if thyme == nil || thyme.Initiative.MorningBrief != "07:30" || !thyme.Initiative.DueReminders {
		t.Fatalf("thyme's check-ins: %+v", thyme)
	}

	// A member can turn on their own solo sprig's, not a family sprig's.
	member := &turn{user: kim, char: f.char, chatRef: "web:2", isDM: true, userText: "a bill tracker with reminders"}
	if _, err := f.e.toolStarterSprigs(ctx, member, json.RawMessage(`{"action":"add","starter":"bill-tracker","check_ins":true}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.internalAction(ctx, kim, member.afterReply[0].approval); err != nil {
		t.Fatal(err)
	}
	if clover, _ := f.e.Chars.Get("clover"); clover == nil || !clover.Initiative.DueReminders || clover.BelongsTo != kim.ID {
		t.Fatalf("clover: %+v", clover)
	}
	member = &turn{user: kim, char: f.char, chatRef: "web:2", isDM: true, userText: "another chore chart"}
	if _, err := f.e.toolStarterSprigs(ctx, member, json.RawMessage(`{"action":"add","starter":"chore-chart","id":"sorrel","name":"Sorrel","check_ins":true}`)); err == nil || !strings.Contains(err.Error(), "admin") {
		t.Fatalf("a member turned on a family sprig's check-ins: %v", err)
	}

	// Kid starters have none to turn on.
	hh, _ := FindStarterSprig("homework-helper")
	if _, err := PrepareStarter(ctx, f.e.DB, f.e.Chars.Dir(), f.e.sprigNameTaken, hh,
		StarterOptions{Owner: f.user.ID, Kids: []int64{mia.ID}, Parents: []int64{f.user.ID}, CheckIns: true}); err == nil {
		t.Fatal("a kid starter took check-ins")
	}
}

func TestStarterCheckInsMetaChecks(t *testing.T) {
	ok := starterMeta{Summary: "s", Offer: "o", Tiers: []string{character.TierFamily}, DailyUSD: 0.5,
		CheckIns: StarterCheckIns{MorningBrief: "07:30", EventReminders: 30, DueReminders: true}}
	if err := ok.check(); err != nil {
		t.Fatal(err)
	}
	for name, m := range map[string]starterMeta{
		"kid":       {Summary: "s", Offer: "o", Tiers: []string{character.TierKid}, DailyUSD: 0.5, CheckIns: StarterCheckIns{DueReminders: true}},
		"bad time":  {Summary: "s", Offer: "o", Tiers: []string{character.TierFamily}, DailyUSD: 0.5, CheckIns: StarterCheckIns{MorningBrief: "breakfast"}},
		"too short": {Summary: "s", Offer: "o", Tiers: []string{character.TierFamily}, DailyUSD: 0.5, CheckIns: StarterCheckIns{EventReminders: 1}},
	} {
		if err := m.check(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
