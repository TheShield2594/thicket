package app

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/theshield2594/thicket/internal/backup"
	"github.com/theshield2594/thicket/internal/server"
	"github.com/theshield2594/thicket/internal/settings"
	"github.com/theshield2594/thicket/internal/store"
)

// backups keeps the "server is running" mark fresh (restore looks for it)
// and writes scheduled full backups when backup_dir is set.
func (a *App) backups(ctx context.Context) {
	beat := func() {
		if err := backup.Heartbeat(a.Cfg.DataDir); err != nil {
			a.Log.Error("heartbeat", "err", err)
		}
	}
	beat()
	// A backup missed while Thicket was stopped is taken at start; one
	// that failed is tried again at its next time, not every tick.
	lastTry := time.Time{}
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			beat()
			if a.backupDue(ctx, now, lastTry) {
				lastTry = now
				a.RunScheduledBackup(ctx)
			}
		}
	}
}

// backupDue reports whether the schedule has passed since the newest
// backup in backup_dir (or since the last attempt).
func (a *App) backupDue(ctx context.Context, now, lastTry time.Time) bool {
	dir := a.Settings.String(ctx, "backup_dir")
	if dir == "" {
		return false
	}
	sched, err := settings.BackupSchedule(a.Settings.String(ctx, "backup_schedule"))
	if err != nil {
		return false // Set refuses a bad line; nothing to run on
	}
	last := lastTry
	if info, ok := backup.Latest(dir); ok && info.At.After(last) {
		last = info.At
	}
	if last.IsZero() {
		return true // switched on and never run: take the first one now
	}
	return !now.Before(sched.Next(last))
}

// RunScheduledBackup writes one full backup into backup_dir now. A
// failure is logged, reported to the admins and put in the needs-you
// inbox (one item, bumped while it keeps failing).
func (a *App) RunScheduledBackup(ctx context.Context) (backup.Info, error) {
	dir := a.Settings.String(ctx, "backup_dir")
	info, err := backup.Scheduled(ctx, a.DB, a.Cfg.DataDir, a.Cfg.CharactersDir, dir, a.Settings.Int(ctx, "backup_keep"))
	a.mu.Lock()
	if err != nil {
		a.backupErr = shortErr(err)
	} else {
		a.backupErr = ""
	}
	a.mu.Unlock()
	if err != nil {
		a.Log.Error("scheduled backup failed", "dir", dir, "err", err)
		msg := "The scheduled backup to " + dir + " failed: " + shortErr(err)
		if admins, aerr := a.DB.Admins(ctx); aerr == nil {
			for _, u := range admins {
				if _, ierr := a.DB.AddInbox(ctx, store.InboxItem{UserID: u.ID, Kind: store.InboxFailed, Ref: "backup",
					Title: "Scheduled backup failed", Detail: msg}); ierr != nil {
					a.Log.Error("backup inbox item", "err", ierr)
				}
			}
		}
		a.Engine.Notify(ctx, "backup-fail", 24*time.Hour, "error", msg+" It's in your needs-you list; I'll try again at the next scheduled time.")
		return info, err
	}
	a.Log.Info("scheduled backup written", "path", info.Path, "bytes", info.Size)
	return info, nil
}

// backupCheck is the health line for backups: when the last one was and
// how big. It names no path, since /health is public.
func (a *App) backupCheck(ctx context.Context) server.Check {
	a.mu.Lock()
	failed := a.backupErr
	a.mu.Unlock()
	dir := a.Settings.String(ctx, "backup_dir")
	if dir != "" {
		c := server.Check{Optional: true, OK: failed == ""}
		if info, ok := backup.Latest(dir); ok {
			c.Detail = "last full backup " + info.At.UTC().Format("2006-01-02 15:04 UTC") + ", " + humanSize(info.Size)
		} else {
			c.Detail = "scheduled, none written yet"
		}
		if failed != "" {
			c.Detail += "; the latest attempt failed (see needs you)"
		}
		return c
	}
	if info, ok := backup.LatestSnapshot(filepath.Join(a.Cfg.DataDir, "backups")); ok {
		return server.Check{OK: true, Optional: true, Detail: "last database snapshot " + info.At.UTC().Format("2006-01-02 15:04 UTC") + ", " + humanSize(info.Size) + " (full scheduled backups are off: set backup_dir)"}
	}
	return server.Check{OK: true, Optional: true, Detail: "no backup yet (full scheduled backups are off: set backup_dir)"}
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}
