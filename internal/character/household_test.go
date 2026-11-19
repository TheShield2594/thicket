package character

import (
	"strings"
	"testing"
	"time"
)

type person struct {
	id  int64
	kid bool
}

func (p person) PersonID() int64 { return p.id }
func (p person) Child() bool     { return p.kid }

func base(id string) Character {
	return Character{ID: id, Name: strings.ToUpper(id[:1]) + id[1:], Role: "Helps.", Personality: Personality{Traits: []string{"kind"}}, Voice: Voice{Style: []string{"warm"}}}
}

func TestTiersAndWhoMayUse(t *testing.T) {
	adultA, adultB := person{id: 1}, person{id: 2}
	kidX, kidY := person{id: 3, kid: true}, person{id: 4, kid: true}

	family := base("pippa")
	solo := base("comics")
	solo.BelongsTo = 1
	kid := base("bramble")
	kid.Household, kid.Kids, kid.Parents = TierKid, []int64{3}, []int64{1}

	if family.Tier() != TierFamily || solo.Tier() != TierSolo || kid.Tier() != TierKid {
		t.Fatalf("tiers: %s %s %s", family.Tier(), solo.Tier(), kid.Tier())
	}
	cases := []struct {
		c    *Character
		p    person
		want bool
		why  string
	}{
		{&family, adultA, true, "adults use family sprigs"},
		{&family, adultB, true, "every adult"},
		{&family, kidX, false, "kids don't use family sprigs"},
		{&solo, adultA, true, "the owner uses their solo sprig"},
		{&solo, adultB, false, "nobody else does"},
		{&solo, kidX, false, "nor kids"},
		{&kid, kidX, true, "the kid it was set up for"},
		{&kid, kidY, false, "another kid"},
		{&kid, adultA, false, "its parent reads its chats but doesn't chat as the kid"},
		{&kid, adultB, false, "another adult"},
	}
	for _, c := range cases {
		if got := c.c.UsableBy(c.p); got != c.want {
			t.Errorf("%s / person %d: got %v (%s)", c.c.ID, c.p.id, got, c.why)
		}
	}
	if kid.UsableBy(nil) {
		t.Error("nobody can use a sprig")
	}
	if !kid.IsParent(1) || kid.IsParent(2) || family.IsParent(1) {
		t.Error("IsParent")
	}
	if kid.Allows(AllowCalendar) || !family.Allows(AllowCalendar) {
		t.Error("kid sprigs see no adult data by default; others aren't limited by allow")
	}
	kid.Allow = []string{AllowCalendar}
	if !kid.Allows(AllowCalendar) || kid.Allows(AllowFamilyMemory) {
		t.Error("allow is per item")
	}
}

func TestKidSprigValidation(t *testing.T) {
	ok := base("bramble")
	ok.Household, ok.Kids, ok.Parents = TierKid, []int64{3}, []int64{1}
	ok.Tools = []string{"web", "mcp:fortnite/player_stats"}
	ok.Allow = []string{AllowFamilyMemory}
	if p := ok.Validate(); len(p) != 0 {
		t.Fatalf("valid kid sprig refused: %v", p)
	}
	bad := map[string]func(c *Character){
		"schedule scope":        func(c *Character) { c.Tools = []string{"schedule"} },
		"admin scope":           func(c *Character) { c.Tools = []string{"admin"} },
		"calendar as a scope":   func(c *Character) { c.Tools = []string{"calendar"} },
		"a whole integration":   func(c *Character) { c.Tools = []string{"mcp:gmail"} },
		"wildcard tools":        func(c *Character) { c.Tools = []string{"mcp:gmail/*"} },
		"no parents":            func(c *Character) { c.Parents = nil },
		"no kids":               func(c *Character) { c.Kids = nil },
		"kid is own parent":     func(c *Character) { c.Parents = []int64{3} },
		"check-ins":             func(c *Character) { c.Initiative.CheckIns = true },
		"follow-ups":            func(c *Character) { c.Initiative.FollowUps = true },
		"unknown allow":         func(c *Character) { c.Allow = []string{"email"} },
		"host":                  func(c *Character) { c.Host = true },
		"belongs to one person": func(c *Character) { c.BelongsTo = 1 },
		"not a real tier":       func(c *Character) { c.Household = "teen" },
	}
	for name, mod := range bad {
		c := ok
		c.Tools = append([]string(nil), ok.Tools...)
		mod(&c)
		if len(c.Validate()) == 0 {
			t.Errorf("%s: accepted", name)
		}
	}
	fam := base("pippa")
	fam.Kids = []int64{3}
	if len(fam.Validate()) == 0 {
		t.Error("kids on a family sprig accepted")
	}
	solo := base("comics")
	solo.Household = TierSolo
	if len(solo.Validate()) == 0 {
		t.Error("solo sprig without belongs_to accepted")
	}
}

func TestKidLimitsClosed(t *testing.T) {
	at := func(day, clock string) time.Time {
		tm, err := time.Parse("2006-01-02 15:04", day+" "+clock)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}
	// 2026-11-15 is a Sunday.
	l := KidLimits{Hours: "07:00-21:00", SchoolNights: "20:00"}
	for _, c := range []struct{ day, clock, want string }{
		{"2026-11-15", "19:59", ""},             // Sunday evening, before school-night time
		{"2026-11-15", "20:00", "school_night"}, // Sunday is a school night
		{"2026-11-16", "02:00", "school_night"}, // still Sunday night
		{"2026-11-16", "06:30", "hours"},        // Monday morning, before hours
		{"2026-11-16", "07:00", ""},
		{"2026-11-19", "20:30", "school_night"}, // Thursday
		{"2026-11-20", "20:30", ""},             // Friday: not a school night
		{"2026-11-21", "01:00", "hours"},        // Friday night after midnight: hours still apply
		{"2026-11-21", "21:00", "hours"},        // Saturday after hours
	} {
		if got := l.Closed(at(c.day, c.clock)); got != c.want {
			t.Errorf("%s %s: %q, want %q", c.day, c.clock, got, c.want)
		}
	}
	late := KidLimits{SchoolNights: "00:30"}
	for _, c := range []struct{ day, clock, want string }{
		{"2026-11-15", "23:00", ""},
		{"2026-11-16", "00:10", ""},
		{"2026-11-16", "00:45", "school_night"},
		{"2026-11-21", "00:45", ""}, // Friday night
	} {
		if got := late.Closed(at(c.day, c.clock)); got != c.want {
			t.Errorf("late %s %s: %q, want %q", c.day, c.clock, got, c.want)
		}
	}
	if (KidLimits{}).Closed(at("2026-11-16", "03:00")) != "" {
		t.Error("no limits closed something")
	}
	for _, bad := range []KidLimits{{Hours: "7-9"}, {Hours: "08:00-08:00"}, {SchoolNights: "25:00"}, {DailyMinutes: -1}, {DailyMessages: 5000}} {
		if len(bad.Problems()) == 0 {
			t.Errorf("%+v accepted", bad)
		}
	}
}
