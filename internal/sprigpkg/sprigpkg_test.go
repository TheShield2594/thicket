package sprigpkg

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theshield2594/thicket/internal/avatar"
	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/secrets"
	"github.com/theshield2594/thicket/internal/store"
	"gopkg.in/yaml.v3"
)

type install struct {
	dir, chars string
	db         *store.Store
	owner      *store.User
}

func newInstall(t *testing.T) *install {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(context.Background(), filepath.Join(dir, "thicket.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	u, err := db.CreateUser(context.Background(), "web", "o", "Owner", store.RoleOwner)
	if err != nil {
		t.Fatal(err)
	}
	return &install{dir: dir, chars: filepath.Join(dir, "characters"), db: db, owner: u}
}

const (
	storedSecret = "hunter2-correct-horse-battery"
	memoryText   = "Sam's gate code is 4471, keep it between us"
)

// pngWithText is a small PNG carrying a text chunk (metadata).
func pngWithText(t *testing.T, key, text string) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	img.Set(1, 1, color.RGBA{90, 164, 105, 255})
	var b bytes.Buffer
	must(t, png.Encode(&b, img))
	raw := b.Bytes()
	data := append([]byte(key+"\x00"), text...)
	chunk := make([]byte, 0, len(data)+12)
	chunk = binary.BigEndian.AppendUint32(chunk, uint32(len(data)))
	chunk = append(chunk, "tEXt"...)
	chunk = append(chunk, data...)
	chunk = binary.BigEndian.AppendUint32(chunk, crc32.ChecksumIEEE(append([]byte("tEXt"), data...)))
	iend := len(raw) - 12 // the IEND chunk is last
	return append(append(append([]byte{}, raw[:iend]...), chunk...), raw[iend:]...)
}

// fern is a solo sprig on the source install with everything a package
// carries, plus things it must leave behind.
func (in *install) addFern(t *testing.T, extra string) {
	t.Helper()
	ctx := context.Background()
	c := &character.Character{ID: "fern", Name: "Fern", Role: "Keeps the garden on track.", Color: "#5aa469", Avatar: "fern.png",
		Personality: character.Personality{Traits: []string{"patient", "earthy"}}, Voice: character.Voice{Style: []string{"short sentences"}},
		Tools: []string{"schedule", "web", "mcp:gmail/*", "admin"}, BelongsTo: in.owner.ID, Household: character.TierSolo,
		Body: "Fern follows the watering-plan skill every morning." + extra}
	md, err := character.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	must(t, os.MkdirAll(filepath.Join(in.chars, "fern", "voice_tests"), 0o755))
	must(t, os.WriteFile(filepath.Join(in.chars, "fern.md"), md, 0o644))
	must(t, os.WriteFile(filepath.Join(in.chars, "fern.png"), pngWithText(t, "Comment", "wifi password "+storedSecret), 0o644))
	must(t, os.WriteFile(avatar.Path(in.chars, "fern"), avatar.Marshal(avatar.Default("fern", "#5aa469")), 0o644))
	vt, _ := yaml.Marshal(character.VoiceTest{Prompt: "hi!", Reference: "Morning. The tomatoes are thirsty."})
	must(t, os.WriteFile(filepath.Join(in.chars, "fern", "voice_tests", "01-hello.yaml"), vt, 0o644))
	// Not part of a sprig: must never be packaged.
	must(t, os.WriteFile(filepath.Join(in.chars, "fern", "notes.txt"), []byte("private"), 0o644))
	if _, err := in.db.SaveSkill(ctx, store.Skill{ID: "watering-plan", Name: "Watering plan", Description: "water by weather", Body: "1. check rain\n2. water"}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := in.db.SaveSkill(ctx, store.Skill{ID: "taxes", Name: "Taxes", Description: "unrelated", Body: "file them"}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := in.db.CreateJob(ctx, store.Job{UserID: in.owner.ID, Character: "fern", Name: "morning check", Schedule: "0 7 * * *", Prompt: "Check the forecast and say what to water.", Kind: store.JobCron}); err != nil {
		t.Fatal(err)
	}
	if _, err := in.db.CreateJob(ctx, store.Job{UserID: in.owner.ID, Character: "fern", Name: "hook", Prompt: "x", Kind: store.JobWebhook, HookHash: "abc"}); err != nil {
		t.Fatal(err)
	}
	paused, err := in.db.CreateJob(ctx, store.Job{UserID: in.owner.ID, Character: "fern", Name: "paused one", Schedule: "0 9 * * *", Prompt: "y", Kind: store.JobCron})
	must(t, err)
	must(t, in.db.SetJobEnabled(ctx, paused.ID, false))
	if _, err := in.db.ApplyMemoryChanges(ctx, 0, []store.MemoryChange{{Action: "add", Memory: store.Memory{UserID: in.owner.ID, Character: "fern", Kind: "fact", Content: memoryText, Source: "user"}}}); err != nil {
		t.Fatal(err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (in *install) export(t *testing.T) (*Package, error) {
	ctx := context.Background()
	jobs, _ := in.db.CharacterJobs(ctx, "fern")
	lib, _ := in.db.Skills(ctx)
	red := secrets.NewRedactor()
	red.Add(storedSecret)
	return Export(Source{CharsDir: in.chars, ID: "fern", Jobs: jobs, Library: lib, Version: "test", LooksSecret: func(s string) bool { return red.Redact(s) != s }})
}

func unpack(t *testing.T, b []byte) map[string]string {
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	out := map[string]string{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		c, _ := io.ReadAll(tr)
		out[h.Name] = string(c)
	}
}

func TestExportImportRoundTrip(t *testing.T) {
	ctx := context.Background()
	src := newInstall(t)
	src.addFern(t, "")
	p, err := src.export(t)
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	files := unpack(t, b)
	var names []string
	for n, c := range files {
		names = append(names, n)
		for _, leak := range []string{memoryText, storedSecret, "notes.txt", "paused one"} {
			if strings.Contains(c, leak) {
				t.Fatalf("%s leaked %q", n, leak)
			}
		}
	}
	for _, want := range []string{"manifest.json", "character.md", "sprig/avatar.json", "sprig/voice_tests/01-hello.yaml", "avatar.png", "skills.json", "routines.json"} {
		if _, ok := files[want]; !ok {
			t.Fatalf("%s missing; package has %v", want, names)
		}
	}
	if len(p.Omitted) != 1 || !strings.Contains(p.Omitted[0], "paused one") {
		t.Fatalf("omitted: %v", p.Omitted)
	}
	if strings.Contains(files["character.md"], "belongs_to") || strings.Contains(files["skills.json"], "taxes") || strings.Contains(files["routines.json"], "hook") {
		t.Fatalf("packaged too much:\n%s\n%s\n%s", files["character.md"], files["skills.json"], files["routines.json"])
	}

	// A fresh install.
	dst := newInstall(t)
	got, err := Read(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	stripped, err := got.Prepare("", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(stripped, ",") != "mcp:gmail/*,admin" || strings.Join(got.Char.Tools, ",") != "schedule,web" {
		t.Fatalf("tools: kept %v, stripped %v", got.Char.Tools, stripped)
	}
	// A sprig once called fern left a level behind on this install.
	must(t, dst.db.SetToolLevel(ctx, "fern", "web_search", store.LevelAct))
	plans, err := got.PlanSkills(ctx, dst.db, nil)
	must(t, err)
	res, err := got.Install(ctx, dst.db, dst.chars, InstallOptions{Owner: dst.owner.ID, Plans: plans, CanWriteSkills: true})
	if err != nil {
		t.Fatal(err)
	}
	reg := character.NewRegistry(dst.chars, dst.db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	must(t, reg.Load())
	c, ok := reg.Get("fern")
	if !ok || c.CreatedBy != "import" || c.Tier() != character.TierFamily || c.Avatar != "fern.png" {
		t.Fatalf("imported sprig: %+v (errors %v)", c, reg.Errors())
	}
	tests, err := character.LoadVoiceTests(dst.chars, "fern")
	if err != nil || len(tests) != 1 || tests[0].Reference != "Morning. The tomatoes are thirsty." {
		t.Fatalf("voice tests: %v %+v", err, tests)
	}
	for _, tool := range []string{"web_search", "schedule_job"} {
		if lv, _ := dst.db.ToolLevel(ctx, "fern", tool, true); lv != store.LevelDraft {
			t.Fatalf("%s level %q, want draft", tool, lv)
		}
	}
	jobs, _ := dst.db.CharacterJobs(ctx, "fern")
	if len(jobs) != 1 || jobs[0].Schedule != "0 7 * * *" || jobs[0].UserID != dst.owner.ID || len(res.Routines) != 1 {
		t.Fatalf("routines: %+v", jobs)
	}
	if k, err := dst.db.SkillByID(ctx, "watering-plan", 0); err != nil || k.Version != 1 {
		t.Fatalf("skill: %v %+v", err, k)
	}
	if mems, _ := dst.db.ExactToolLevel(ctx, "fern", "*"); mems != store.LevelDraft {
		t.Fatalf("sprig-wide level %q", mems)
	}
	// Importing again under the same id is refused with a suggestion.
	again, _ := Read(bytes.NewReader(b))
	_, _ = again.Prepare("", "")
	var clash *Clash
	if err := CheckClash(again.Char, dst.chars, nil); !errors.As(err, &clash) || clash.Suggest != "fern-2" {
		t.Fatalf("clash: %v", err)
	}
}

func TestExportRefusesSecrets(t *testing.T) {
	for name, extra := range map[string]string{
		"stored secret": " The shed password is " + storedSecret + ".",
		"key shape":     " Use sk-or-v1-0123456789abcdef0123456789abcdef for the API.",
	} {
		t.Run(name, func(t *testing.T) {
			src := newInstall(t)
			src.addFern(t, extra)
			_, err := src.export(t)
			if err == nil || !strings.Contains(err.Error(), "character.md") {
				t.Fatalf("export with a secret: %v", err)
			}
			if strings.Contains(err.Error(), storedSecret) {
				t.Fatal("the error repeats the secret")
			}
		})
	}
}

// repack rebuilds an archive after changing its files.
func repack(t *testing.T, files map[string]string) []byte {
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for n, c := range files {
		must(t, tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(c)), Typeflag: tar.TypeReg}))
		_, _ = tw.Write([]byte(c))
	}
	must(t, tw.Close())
	must(t, gz.Close())
	return b.Bytes()
}

func TestReadRefusesTampering(t *testing.T) {
	src := newInstall(t)
	src.addFern(t, "")
	p, err := src.export(t)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := p.Bytes()
	for name, change := range map[string]func(map[string]string){
		"changed file":  func(f map[string]string) { f["character.md"] += "\nextra" },
		"unlisted file": func(f map[string]string) { f["sprig/voice_tests/99-x.yaml"] = "prompt: hi\n" },
		"foreign path":  func(f map[string]string) { f["../memories.json"] = "[]" },
		"secret store":  func(f map[string]string) { f["secret.key"] = "x" },
		"no manifest":   func(f map[string]string) { delete(f, "manifest.json") },
		"newer format": func(f map[string]string) {
			f["manifest.json"] = strings.Replace(f["manifest.json"], `"format": 1`, `"format": 3`, 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			files := unpack(t, b)
			change(files)
			if _, err := Read(bytes.NewReader(repack(t, files))); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestChangedSkillIsANewVersion(t *testing.T) {
	ctx := context.Background()
	src := newInstall(t)
	src.addFern(t, "")
	p, _ := src.export(t)
	b, _ := p.Bytes()
	dst := newInstall(t)
	if _, err := dst.db.SaveSkill(ctx, store.Skill{ID: "watering-plan", Name: "Watering plan", Description: "mine", Body: "my own steps"}, 0); err != nil {
		t.Fatal(err)
	}
	got, _ := Read(bytes.NewReader(b))
	if _, err := got.Prepare("", ""); err != nil {
		t.Fatal(err)
	}
	plans, _ := got.PlanSkills(ctx, dst.db, nil)
	if len(plans) != 1 || plans[0].Action != "new version" {
		t.Fatalf("plan: %+v", plans)
	}
	if _, err := got.Install(ctx, dst.db, dst.chars, InstallOptions{Owner: dst.owner.ID, Plans: plans, CanWriteSkills: true}); err != nil {
		t.Fatal(err)
	}
	hist, _ := dst.db.SkillHistory(ctx, "watering-plan")
	if len(hist) != 2 {
		t.Fatalf("history: %+v", hist)
	}
	if old, _ := dst.db.SkillByID(ctx, "watering-plan", 1); old.Body != "my own steps" {
		t.Fatal("the existing version was overwritten")
	}
}

func TestKidSprigsAreNotPackaged(t *testing.T) {
	in := newInstall(t)
	c := &character.Character{ID: "pip", Name: "Pip", Role: "A kid's sprig.", Household: character.TierKid, Kids: []int64{2}, Parents: []int64{1},
		Personality: character.Personality{Traits: []string{"kind"}}, Voice: character.Voice{Style: []string{"gentle"}}}
	md, _ := character.Marshal(c)
	must(t, os.MkdirAll(in.chars, 0o755))
	must(t, os.WriteFile(filepath.Join(in.chars, "pip.md"), md, 0o644))
	if _, err := Export(Source{CharsDir: in.chars, ID: "pip", LooksSecret: func(string) bool { return false }}); err == nil {
		t.Fatal("kid sprig exported")
	}
}

func exportedFern(t *testing.T) []byte {
	src := newInstall(t)
	src.addFern(t, "")
	p, err := src.export(t)
	must(t, err)
	b, err := p.Bytes()
	must(t, err)
	return b
}

func prepared(t *testing.T, b []byte) *Package {
	p, err := Read(bytes.NewReader(b))
	must(t, err)
	_, err = p.Prepare("", "")
	must(t, err)
	return p
}

func TestNonAdminsDontWriteSkills(t *testing.T) {
	ctx := context.Background()
	dst := newInstall(t)
	p := prepared(t, exportedFern(t))
	plans, _ := p.PlanSkills(ctx, dst.db, nil)
	if pv, _ := p.Preview(plans, nil, false); !strings.Contains(pv, SkillsNeedAdmin) {
		t.Fatalf("preview doesn't say the skill won't come:\n%s", pv)
	}
	res, err := p.Install(ctx, dst.db, dst.chars, InstallOptions{Owner: dst.owner.ID, Plans: plans})
	must(t, err)
	if _, err := dst.db.SkillByID(ctx, "watering-plan", 0); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a member's import wrote a shared skill: %v", err)
	}
	if len(res.Skills) != 1 || !strings.Contains(res.Skills[0], SkillsNeedAdmin) {
		t.Fatalf("skills: %v", res.Skills)
	}
}

func TestPreviewShowsSkillBodies(t *testing.T) {
	ctx := context.Background()
	dst := newInstall(t)
	_, err := dst.db.SaveSkill(ctx, store.Skill{ID: "watering-plan", Name: "Watering plan", Description: "water by weather", Body: "my own steps"}, 0)
	must(t, err)
	p := prepared(t, exportedFern(t))
	plans, _ := p.PlanSkills(ctx, dst.db, nil)
	pv, _ := p.Preview(plans, nil, true)
	if !strings.Contains(pv, "-my own steps") || !strings.Contains(pv, "+1. check rain") {
		t.Fatalf("no skill diff:\n%s", pv)
	}
}

func TestSkillLibraryChangedSincePreview(t *testing.T) {
	ctx := context.Background()
	dst := newInstall(t)
	p := prepared(t, exportedFern(t))
	plans, _ := p.PlanSkills(ctx, dst.db, nil) // "new"
	_, err := dst.db.SaveSkill(ctx, store.Skill{ID: "watering-plan", Name: "W", Description: "d", Body: "someone else's"}, 0)
	must(t, err)
	if _, err := p.Install(ctx, dst.db, dst.chars, InstallOptions{Owner: dst.owner.ID, Plans: plans, CanWriteSkills: true}); err == nil {
		t.Fatal("installed a plan nobody saw")
	}
	if _, err := os.Stat(filepath.Join(dst.chars, "fern.md")); err == nil {
		t.Fatal("the sprig went live")
	}
	if k, _ := dst.db.SkillByID(ctx, "watering-plan", 0); k.Body != "someone else's" {
		t.Fatal("the skill was changed")
	}
}

func TestInvalidSkillRefusedBeforeWriting(t *testing.T) {
	ctx := context.Background()
	dst := newInstall(t)
	p := prepared(t, exportedFern(t))
	bad := func(store.Skill) error { return errors.New("bad steps") }
	if _, err := p.PlanSkills(ctx, dst.db, bad); err == nil || !strings.Contains(err.Error(), "watering-plan") {
		t.Fatalf("invalid skill: %v", err)
	}
}

func TestConcurrentImportOfOneID(t *testing.T) {
	ctx := context.Background()
	dst := newInstall(t)
	p := prepared(t, exportedFern(t))
	plans, _ := p.PlanSkills(ctx, dst.db, nil)
	must(t, os.MkdirAll(dst.chars, 0o755))
	must(t, os.WriteFile(filepath.Join(dst.chars, "fern"+lockSuffix), nil, 0o644))
	if _, err := p.Install(ctx, dst.db, dst.chars, InstallOptions{Owner: dst.owner.ID, Plans: plans}); err == nil {
		t.Fatal("a second import of fern went ahead")
	}
	if err := CheckClash(p.Char, dst.chars, nil); err == nil {
		t.Fatal("an id being imported isn't taken")
	}
}

func TestSuggestionFitsTheIDLimit(t *testing.T) {
	long := "a" + strings.Repeat("b", 31)
	got := freeName(long, "-", maxIDLen, func(s string) bool { return s == long })
	c := &character.Character{ID: got, Name: "X", Role: "r", Personality: character.Personality{Traits: []string{"t"}}, Voice: character.Voice{Style: []string{"s"}}}
	if len(got) > maxIDLen || len(c.Validate()) > 0 {
		t.Fatalf("suggestion %q", got)
	}
}

func TestPictureMetadataIsStripped(t *testing.T) {
	p := prepared(t, exportedFern(t))
	if p.Avatar == nil || bytes.Contains(p.Avatar, []byte("tEXt")) || bytes.Contains(p.Avatar, []byte(storedSecret)) {
		t.Fatal("picture missing, or its metadata came along")
	}
}
