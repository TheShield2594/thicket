package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/theshield2594/thicket/internal/config"
	"github.com/theshield2594/thicket/internal/secrets"
	"github.com/theshield2594/thicket/internal/store"
)

const secretsUsage = `usage:
  thicket secrets list                 names of stored secrets (never values)
  thicket secrets set NAME             store a secret, reading the value from stdin
  thicket secrets delete NAME          remove a stored secret
  thicket secrets rotate-key [--out FILE]
                                       re-encrypt every secret under a new master key`

func secretsCmd(args []string, in io.Reader, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(secretsUsage)
	}
	cfg := loadConfig()
	db, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	key, src, err := secrets.ResolveKey(cfg.SecretKey, cfg.SecretKeyFile, cfg.DataDir)
	if err != nil {
		return err
	}
	sec, err := secrets.New(db, key, secrets.NewRedactor())
	if err != nil {
		return err
	}
	ctx := context.Background()
	switch args[0] {
	case "list", "ls":
		return secretsList(ctx, cfg, db, sec, out)
	case "set":
		if len(args) != 2 {
			return errors.New("usage: thicket secrets set NAME  (the value is read from stdin)")
		}
		return secretsSet(ctx, sec, args[1], in, out)
	case "delete", "rm", "unset":
		if len(args) != 2 {
			return errors.New("usage: thicket secrets delete NAME")
		}
		ok, err := sec.Stored(ctx, args[1])
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("no stored secret called %q", args[1])
		}
		if err := sec.Delete(ctx, args[1]); err != nil {
			return err
		}
		fmt.Fprintf(out, "Deleted %s.\n", args[1])
		if e := envFor(cfg, args[1]); e != "" {
			fmt.Fprintf(out, "%s is still set in the environment, so it will be stored again at the next start.\n", e)
		}
		return nil
	case "rotate-key":
		return secretsRotate(ctx, cfg, sec, src, args[1:], out)
	}
	return errors.New(secretsUsage)
}

// envFor is the environment variable a stored secret is imported from,
// if it's set.
func envFor(cfg *config.Config, name string) string {
	for _, e := range cfg.EnvSecrets() {
		if e.Name == name && e.Value != "" {
			return e.Env
		}
	}
	return ""
}

func secretsList(ctx context.Context, cfg *config.Config, db *store.Store, sec *secrets.Store, out io.Writer) error {
	names, err := db.SecretNames(ctx)
	if err != nil {
		return err
	}
	stored := map[string]bool{}
	for _, n := range names {
		stored[n] = true
	}
	for _, e := range cfg.EnvSecrets() {
		if e.Value != "" && !stored[e.Name] {
			names = append(names, e.Name)
		}
	}
	if len(names) == 0 {
		fmt.Fprintln(out, "No secrets stored yet.")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tWHERE")
	for _, n := range names {
		where := "stored"
		switch env := envFor(cfg, n); {
		case !stored[n]:
			where = "environment only (" + env + "): stored at the next start"
		case env != "":
			where = "stored; also in the environment (" + env + "), which you can remove"
		}
		if stored[n] && !sec.Has(ctx, n) {
			where += "; CAN'T BE DECRYPTED with the current key"
		}
		fmt.Fprintf(tw, "%s\t%s\n", n, where)
	}
	return tw.Flush()
}

func secretsSet(ctx context.Context, sec *secrets.Store, name string, in io.Reader, out io.Writer) error {
	if !secrets.ValidName(name) {
		return fmt.Errorf("invalid name %q: use 2-48 lowercase letters, digits and underscores (e.g. discord_token)", name)
	}
	if f, ok := in.(*os.File); ok {
		if st, err := f.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 {
			fmt.Fprintf(out, "Paste the value for %s and press Enter (it won't be shown again):\n", name)
		}
	}
	b, err := io.ReadAll(io.LimitReader(in, 16<<10+1))
	if err != nil {
		return err
	}
	if len(b) > 16<<10 {
		return errors.New("that's too long for a secret (16 KB at most)")
	}
	value := strings.TrimSpace(string(b))
	if value == "" {
		return errors.New("no value given on stdin")
	}
	if err := sec.Put(ctx, name, value); err != nil {
		return err
	}
	fmt.Fprintf(out, "Stored %s encrypted.\n", name)
	switch name {
	case secrets.DiscordToken, secrets.TelegramToken:
		fmt.Fprintln(out, "Restart Thicket for the chat app to use it.")
	}
	return nil
}

func secretsRotate(ctx context.Context, cfg *config.Config, sec *secrets.Store, src secrets.KeySource, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("rotate-key", flag.ContinueOnError)
	outPath := fs.String("out", "", "where to write the new key (required when the key comes from THICKET_SECRET_KEY or THICKET_SECRET_KEY_FILE)")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return errors.New("usage: thicket secrets rotate-key [--out FILE]")
	}
	if src != secrets.KeyFromDataDir && *outPath == "" {
		return errors.New("the master key comes from THICKET_SECRET_KEY or THICKET_SECRET_KEY_FILE, which Thicket can't change: " +
			"run again with --out FILE to get the new key in a file, then put it where the old one was")
	}
	newKey, err := secrets.NewKey()
	if err != nil {
		return err
	}
	// The new key is written before anything is re-encrypted, so it can't
	// be lost; the old one is kept until the switch is done.
	keyPath := secrets.KeyPath(cfg.DataDir)
	pending := keyPath + ".new"
	if *outPath != "" {
		pending = *outPath
	}
	if err := secrets.WriteKeyFile(pending, newKey); err != nil {
		return fmt.Errorf("write the new key: %w", err)
	}
	n, err := sec.RotateKey(ctx, newKey)
	if err != nil {
		if *outPath == "" {
			os.Remove(pending)
		}
		return err
	}
	if src == secrets.KeyFromDataDir {
		old := keyPath + ".old"
		if err := os.Rename(keyPath, old); err != nil {
			return fmt.Errorf("re-encrypted %d secrets, but couldn't move the old key aside (%v): rename %s to %s yourself before restarting", n, err, pending, keyPath)
		}
		if err := os.Rename(pending, keyPath); err != nil {
			return fmt.Errorf("re-encrypted %d secrets, but couldn't put the new key in place (%v): rename %s to %s yourself before restarting", n, err, pending, keyPath)
		}
		fmt.Fprintf(out, "Re-encrypted %d secret(s) under a new key in %s. The old key is in %s: delete it once Thicket runs fine.\n", n, keyPath, old)
		if *outPath != "" {
			fmt.Fprintln(out, "(--out is ignored when the key lives in the data directory.)")
		}
	} else {
		which := "THICKET_SECRET_KEY"
		if src == secrets.KeyFromFile {
			which = "the file THICKET_SECRET_KEY_FILE names (" + cfg.SecretKeyFile + ")"
		}
		fmt.Fprintf(out, "Re-encrypted %d secret(s). The new key is in %s: put its contents in %s now, then delete that file.\n", n, pending, which)
	}
	fmt.Fprintln(out, "Restart Thicket now: until it restarts with the new key, it can't read the secrets.")
	fmt.Fprintln(out, "Old exports made with --include-key still hold the old key and old ciphertexts; they restore as they were.")
	return nil
}
