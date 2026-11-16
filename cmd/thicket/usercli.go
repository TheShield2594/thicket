package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"github.com/theshield2594/thicket/internal/auth"
	"github.com/theshield2594/thicket/internal/config"
	"github.com/theshield2594/thicket/internal/store"
)

const userUsage = `usage:
  thicket user list                    everyone, with their role and how they sign in
  thicket user add USERNAME [--role member|admin] [--name NAME] [--user ID]
                                       make a web login (reads the password from stdin);
                                       --user gives an existing user (say, from Discord) the login
  thicket user passwd USERNAME         set a new password (signs out their sessions)
  thicket user email ID ADDRESS        the email single sign-on matches them by ("" clears it)
  thicket user invite [--role member|admin] [--name NAME] [--user ID] [--ttl 72h]
                                       print a one-time invite link`

func userCmd(args []string, in io.Reader, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(userUsage)
	}
	cfg := loadConfig()
	db, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx := context.Background()
	switch args[0] {
	case "list", "ls":
		return userList(ctx, db, out)
	case "add":
		return userAdd(ctx, db, args[1:], in, out)
	case "passwd", "password":
		if len(args) != 2 {
			return errors.New("usage: thicket user passwd USERNAME")
		}
		u, _, err := db.LoginByUsername(ctx, args[1])
		if err != nil {
			return fmt.Errorf("no login called %q (thicket user list shows them)", args[1])
		}
		pw, err := readNewPassword(in, out)
		if err != nil {
			return err
		}
		hash, err := auth.HashPassword(pw)
		if err != nil {
			return err
		}
		if err := db.SetPassword(ctx, u.ID, hash); err != nil {
			return err
		}
		n, _ := db.RevokeUserSessions(ctx, u.ID, "")
		fmt.Fprintf(out, "Password changed for %s. Signed out %d session(s).\n", args[1], n)
		return nil
	case "email":
		if len(args) != 3 {
			return errors.New("usage: thicket user email ID ADDRESS")
		}
		var id int64
		if _, err := fmt.Sscan(args[1], &id); err != nil {
			return errors.New("usage: thicket user email ID ADDRESS (ID from thicket user list)")
		}
		if _, err := db.UserByID(ctx, id); err != nil {
			return fmt.Errorf("user %d: %w", id, err)
		}
		if err := db.SetUserEmail(ctx, id, args[2]); err != nil {
			return err
		}
		fmt.Fprintf(out, "Single sign-on with the verified email %q now signs in as user %d.\n", args[2], id)
		return nil
	case "invite":
		return userInvite(ctx, cfg, db, args[1:], out)
	}
	return errors.New(userUsage)
}

func userList(ctx context.Context, db *store.Store, out io.Writer) error {
	users, err := db.ListUsers(ctx)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tROLE\tSIGNS IN WITH")
	for _, u := range users {
		var ways []string
		if l, err := db.LoginFor(ctx, u.ID); err == nil {
			w := "username " + l.Username
			if !l.HasPassword {
				w += " (no password)"
			}
			ways = append(ways, w)
		}
		if ok, _ := db.HasOIDC(ctx, u.ID); ok {
			ways = append(ways, "single sign-on")
		}
		ids, _ := db.Identities(ctx, u.ID)
		for _, i := range ids {
			if i.Channel != "web" {
				ways = append(ways, i.Channel)
			}
		}
		if len(ways) == 0 {
			ways = []string{"sign-in links only"}
		}
		name := u.Name
		if name == "" {
			name = "-"
		}
		if u.Email != "" {
			name += " <" + u.Email + ">"
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\n", u.ID, name, u.Role, strings.Join(ways, ", "))
	}
	return tw.Flush()
}

func userAdd(ctx context.Context, db *store.Store, args []string, in io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("user add", flag.ContinueOnError)
	role := fs.String("role", "member", "member or admin")
	name := fs.String("name", "", "what sprigs call them")
	userID := fs.Int64("user", 0, "give this existing user the login instead of making a new one")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: thicket user add USERNAME [--role member|admin] [--name NAME] [--user ID]")
	}
	username := pos[0]
	if !store.ValidUsername(username) {
		return errors.New("usernames are 2 to 32 letters, digits, dots, dashes or underscores")
	}
	if *role != "member" && *role != "admin" {
		return errors.New("--role must be member or admin")
	}
	if _, _, err := db.LoginByUsername(ctx, username); err == nil {
		return fmt.Errorf("%q is taken", username)
	}
	var u *store.User
	if *userID != 0 {
		if u, err = db.UserByID(ctx, *userID); err != nil {
			return fmt.Errorf("user %d: %w", *userID, err)
		}
		if _, err := db.LoginFor(ctx, u.ID); err == nil {
			return fmt.Errorf("user %d already has a login (thicket user passwd changes its password)", u.ID)
		}
	}
	pw, err := readNewPassword(in, out)
	if err != nil {
		return err
	}
	hash, err := auth.HashPassword(pw)
	if err != nil {
		return err
	}
	if u == nil {
		if u, err = db.CreateWebUser(ctx, *name, *role); err != nil {
			return err
		}
	}
	if err := db.SetLogin(ctx, u.ID, username, hash); err != nil {
		return err
	}
	fmt.Fprintf(out, "%s can sign in to the web app as %q (user %d, %s).\n", orNone(u.Name), username, u.ID, u.Role)
	return nil
}

func userInvite(ctx context.Context, cfg *config.Config, db *store.Store, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("user invite", flag.ContinueOnError)
	role := fs.String("role", "member", "member or admin")
	name := fs.String("name", "", "what sprigs call them")
	userID := fs.Int64("user", 0, "give this existing user a login instead of making a new one")
	ttl := fs.Duration("ttl", 72*time.Hour, "how long the link stays valid (up to 720h)")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 || *ttl <= 0 || *ttl > 30*24*time.Hour || (*role != "member" && *role != "admin") {
		return errors.New("usage: thicket user invite [--role member|admin] [--name NAME] [--user ID] [--ttl 72h]")
	}
	if *userID != 0 {
		if _, err := db.UserByID(ctx, *userID); err != nil {
			return fmt.Errorf("user %d: %w", *userID, err)
		}
	}
	token, err := db.CreateInvite(ctx, store.Invite{UserID: *userID, Role: *role, Name: *name}, *ttl)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Invite link, valid once for %s. Send it privately:\n\n  %s/invite#%s\n\n", *ttl, cfg.WebBaseURL(), token)
	if cfg.PublicURL == "" {
		fmt.Fprintln(out, "Set THICKET_PUBLIC_URL if people reach Thicket through another address.")
	}
	return nil
}

// readNewPassword reads a password: typed twice without echo at a
// terminal, or one line from a pipe.
func readNewPassword(in io.Reader, out io.Writer) (string, error) {
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(out, "Password: ")
		a, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(out)
		if err != nil {
			return "", err
		}
		fmt.Fprint(out, "Again: ")
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(out)
		if err != nil {
			return "", err
		}
		if string(a) != string(b) {
			return "", errors.New("the two passwords don't match")
		}
		return string(a), auth.CheckPasswordRules(string(a))
	}
	line, err := bufio.NewReader(io.LimitReader(in, 4096)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	pw := strings.TrimRight(line, "\r\n")
	if pw == "" {
		return "", errors.New("no password given on stdin")
	}
	return pw, auth.CheckPasswordRules(pw)
}
