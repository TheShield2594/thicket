package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/backup"
	"github.com/theshield2594/thicket/internal/config"
	"github.com/theshield2594/thicket/internal/doctor"
	"github.com/theshield2594/thicket/internal/store"
)

func TestScheduledBackups(t *testing.T) {
	a, fc, _, ctx := setup(t)
	a.Engine.HandleIncoming(ctx, dm("hello?")) // makes the owner
	fc.take()
	now := time.Now()

	// Off until a folder is set.
	if a.backupDue(ctx, now, time.Time{}) {
		t.Fatal("backups must be off without backup_dir")
	}
	if c := a.health(ctx)["backup"]; !c.OK || !c.Optional || !strings.Contains(c.Detail, "set backup_dir") {
		t.Fatalf("health with backups off: %+v", c)
	}
	for name, bad := range map[string]string{"backup_dir": "relative/path", "backup_schedule": "every day", "backup_keep": "0"} {
		if err := a.Settings.Set(ctx, name, bad); err == nil {
			t.Errorf("%s = %q should be refused", name, bad)
		}
	}

	dir := filepath.Join(t.TempDir(), "backups")
	for name, v := range map[string]string{"backup_dir": dir, "backup_schedule": "0 3 * * *", "backup_keep": "2"} {
		if err := a.Settings.Set(ctx, name, v); err != nil {
			t.Fatal(err)
		}
	}
	// Switched on and never run: the first one is taken straight away.
	if !a.backupDue(ctx, now, time.Time{}) {
		t.Fatal("the first backup should be due at once")
	}
	info, err := a.RunScheduledBackup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if a.backupDue(ctx, now, now) {
		t.Fatal("a backup just written must not be due again")
	}
	// ...until the schedule's next time has passed.
	if !a.backupDue(ctx, now.Add(25*time.Hour), now) {
		t.Fatal("a day later the next backup is due")
	}
	c := a.health(ctx)["backup"]
	if !c.OK || !strings.Contains(c.Detail, "last full backup") || strings.Contains(c.Detail, dir) {
		t.Fatalf("health after a backup (and it must not name the folder): %+v", c)
	}

	// Done when: a scheduled backup restores into an empty data dir and
	// doctor's database check passes on it.
	f, err := os.Open(info.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	dst := t.TempDir()
	if err := backup.Restore(f, dst, filepath.Join(dst, "characters"), false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "characters", "juniper.md")); err != nil {
		t.Fatalf("the host sprig didn't come back: %v", err)
	}
	cfg := &config.Config{DataDir: dst, CharactersDir: filepath.Join(dst, "characters"), HTTPAddr: "127.0.0.1:1"}
	var dbOK bool
	for _, ch := range (&doctor.Doctor{Cfg: cfg}).Run(ctx) {
		if ch.Name == "database" {
			dbOK = ch.OK
		}
	}
	if !dbOK {
		t.Fatal("doctor's database check fails on the restored backup")
	}

	// A failure is one needs-you item (bumped, not piled up) and a message.
	blocker := filepath.Join(t.TempDir(), "a-file")
	os.WriteFile(blocker, []byte("x"), 0o600)
	if err := a.Settings.Set(ctx, "backup_dir", filepath.Join(blocker, "nope")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := a.RunScheduledBackup(ctx); err == nil {
			t.Fatal("writing under a file should fail")
		}
	}
	items, err := a.DB.OpenInbox(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	var found []store.InboxItem
	for _, it := range items {
		if it.Ref == "backup" {
			found = append(found, it)
		}
	}
	if len(found) != 1 || found[0].Count != 2 || found[0].Kind != store.InboxFailed {
		t.Fatalf("backup failure in the inbox: %+v", found)
	}
	if out := fc.take(); len(out) != 1 || !strings.Contains(out[0].text, "scheduled backup") {
		t.Fatalf("the owner should hear about it once: %+v", out)
	}
	if c := a.health(ctx)["backup"]; c.OK {
		t.Fatalf("health should show the failed backup: %+v", c)
	}
}
