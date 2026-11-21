package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/theshield2594/thicket/internal/config"
	"github.com/theshield2594/thicket/internal/secrets"
	"github.com/theshield2594/thicket/internal/sprigpkg"
	"github.com/theshield2594/thicket/internal/store"
)

const galleryUsage = `usage: thicket gallery COMMAND

Your author key:
  keygen         make your author key for signing gallery packages (an Ed25519
                 key pair; the private key is kept in the encrypted secret store)
  key            show your author key id and fingerprint, to publish
  revoke KEYID   refuse packages signed by an author key from now on
  revoked        list the revoked author keys (yours and the gallery's)

A shared gallery (THICKET_GALLERY_URL):
  list           the gallery's sprigs
  show NAME      one sprig: its author, key fingerprint, scopes and versions
  install NAME [--version N] [--id ID] [--name NAME] [--user ID] [--yes]
                 download it, check it against the index, and import it after
                 showing you everything it will get
  refresh        fetch the gallery's index now (it's fetched daily)

Running a gallery (a repository of packages):
  verify DIR       check every package and index.json, as the gallery's CI does
  build-index DIR  write DIR/index.json from the packages
  build-site DIR [-o OUT]
                   write the static site (default DIR/site) to publish`

func galleryCmd(cfg *config.Config, args []string, out io.Writer) error {
	return galleryCmdIO(cfg, args, os.Stdin, out)
}

func galleryCmdIO(cfg *config.Config, args []string, in io.Reader, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(galleryUsage)
	}
	switch args[0] {
	case "verify", "build-index", "build-site":
		return galleryRepoCmd(args, out)
	case "list", "show", "install", "refresh":
		return galleryIndexCmd(cfg, args, in, out)
	}
	db, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx := context.Background()
	switch args[0] {
	case "keygen":
		fs := flag.NewFlagSet("gallery keygen", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		replace := fs.Bool("replace", false, "")
		if _, err := parseFlags(fs, args[1:]); err != nil {
			return errors.New("usage: thicket gallery keygen [--replace]")
		}
		sec, err := openSecrets(ctx, cfg, db)
		if err != nil {
			return err
		}
		// Only a missing key may be made without --replace: one that is
		// stored but can't be read now (a changed secret key) still exists.
		if _, err := sec.Get(ctx, sprigpkg.GallerySecret); !*replace {
			switch {
			case err == nil:
				return errors.New("you already have an author key (thicket gallery key shows it). A new one can't sign updates of sprigs you've already shared; pass --replace if you really want one")
			case !errors.Is(err, secrets.ErrNotFound):
				return fmt.Errorf("your author key is stored but can't be read (%w); fix that, or pass --replace to make a new one, which can't sign updates of sprigs you've already shared", err)
			}
		}
		seed, pub, err := sprigpkg.NewSigningKey()
		if err != nil {
			return err
		}
		if err := sec.Put(ctx, sprigpkg.GallerySecret, seed); err != nil {
			return err
		}
		fmt.Fprintf(out, "Made your author key %s.\nFingerprint: %s\nPublish the fingerprint where people can check it; the private key stays in the encrypted secret store.\n", sprigpkg.KeyID(pub), sprigpkg.Fingerprint(pub))
		return nil
	case "key":
		sec, err := openSecrets(ctx, cfg, db)
		if err != nil {
			return err
		}
		priv, err := gallerySigningKey(ctx, sec)
		if err != nil {
			return err
		}
		pub := priv.Public().(ed25519.PublicKey)
		fmt.Fprintf(out, "Author key %s\nFingerprint: %s\n", sprigpkg.KeyID(pub), sprigpkg.Fingerprint(pub))
		return nil
	case "revoke":
		if len(args) != 2 || !keyIDRe.MatchString(strings.ToLower(args[1])) {
			return errors.New("usage: thicket gallery revoke KEYID (the 16-character key id a package preview shows)")
		}
		list, err := sprigpkg.RevokedKeys(ctx, db)
		if err != nil {
			return err
		}
		id := strings.ToLower(args[1])
		for _, k := range list {
			if k == id {
				fmt.Fprintf(out, "%s is already revoked.\n", id)
				return nil
			}
		}
		if err := db.SetSetting(ctx, store.GalleryRevokedKey, strings.Join(append(list, id), ",")); err != nil {
			return err
		}
		fmt.Fprintf(out, "Revoked %s: packages signed with it are refused from now on. Sprigs already imported with it stay; remove them if you no longer trust them.\n", id)
		return nil
	case "revoked":
		list, err := sprigpkg.RevokedKeys(ctx, db)
		if err != nil {
			return err
		}
		var fromIndex []string
		for _, k := range galleryClient(cfg).Revoked() {
			if !slices.Contains(list, k) {
				fromIndex = append(fromIndex, k)
			}
		}
		if len(list)+len(fromIndex) == 0 {
			fmt.Fprintln(out, "No author keys are revoked.")
		}
		for _, k := range list {
			fmt.Fprintln(out, k)
		}
		for _, k := range fromIndex {
			fmt.Fprintln(out, k, "(revoked by the gallery)")
		}
		return nil
	}
	return errors.New(galleryUsage)
}

var keyIDRe = regexp.MustCompile(`^[0-9a-f]{16}$`)

func openSecrets(ctx context.Context, cfg *config.Config, db *store.Store) (*secrets.Store, error) {
	key, _, err := secrets.ResolveKey(cfg.SecretKey, cfg.SecretKeyFile, cfg.DataDir)
	if err != nil {
		return nil, err
	}
	return secrets.New(db, key, secrets.NewRedactor())
}

func gallerySigningKey(ctx context.Context, sec *secrets.Store) (ed25519.PrivateKey, error) {
	seed, err := sec.Get(ctx, sprigpkg.GallerySecret)
	if err != nil {
		return nil, errors.New("you have no author key yet: make one with thicket gallery keygen")
	}
	return sprigpkg.ParseSigningKey(seed)
}
