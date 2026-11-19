package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/theshield2594/thicket/internal/sprigpkg"
	"github.com/theshield2594/thicket/internal/store"
)

// Sprig packages sent in chat (`thicket export-sprig` files): the host
// shows everything the sprig will get, exactly as `thicket import-sprig`
// does, and imports it only once the person approves. The model never
// sees the package's contents, only whether it can be imported.

// maxChatPackageBytes caps a package sent in chat: it travels in the
// approval until the person answers.
const maxChatPackageBytes = 2 << 20

type sprigImportArgs struct {
	File    string `json:"file"`
	ID      string `json:"id,omitempty"`
	Name    string `json:"name,omitempty"`
	Package string `json:"package"` // base64 of the file, exactly as shown
}

// readPackage reads and prepares a package, refusing id or name clashes.
func (e *Engine) readPackage(raw []byte, id, name string) (*sprigpkg.Package, []string, error) {
	p, err := sprigpkg.Read(bytes.NewReader(raw))
	if err != nil {
		return nil, nil, err
	}
	stripped, err := p.Prepare(id, name)
	if err != nil {
		return nil, nil, err
	}
	taken := func(s string) bool {
		if _, ok := e.Chars.Get(strings.ToLower(s)); ok {
			return true
		}
		_, ok := e.Chars.FindByName(s)
		return ok
	}
	if err := sprigpkg.CheckClash(p.Char, e.Chars.Dir(), taken); err != nil {
		return nil, nil, err
	}
	return p, stripped, nil
}

func (e *Engine) toolImportSprig(ctx context.Context, t *turn, args json.RawMessage) (string, error) {
	a, err := decode[struct {
		File string `json:"file"`
		ID   string `json:"id"`
		Name string `json:"name"`
	}](args)
	if err != nil {
		return "", err
	}
	raw, file := t.packages[a.File], a.File
	if raw == nil && len(t.packages) == 1 {
		for n, b := range t.packages {
			raw, file = b, n
		}
	}
	if raw == nil {
		var names []string
		for n := range t.packages {
			names = append(names, n)
		}
		sort.Strings(names)
		if len(names) == 0 {
			return "", errors.New("no sprig package came with this message: ask the user to send the .sprig file (made with thicket export-sprig) and call this again")
		}
		return "", fmt.Errorf("no package called %q here; the packages sent are: %s", a.File, strings.Join(names, ", "))
	}
	p, stripped, err := e.readPackage(raw, a.ID, a.Name)
	if err != nil {
		var c *sprigpkg.Clash
		if errors.As(err, &c) {
			return "", fmt.Errorf("%w: ask the user whether to import it as %s %q and pass that as %s", err, c.What, c.Suggest, c.What)
		}
		return "", fmt.Errorf("this package can't be imported: %w", err)
	}
	plans, err := p.PlanSkills(ctx, e.DB)
	if err != nil {
		return "", err
	}
	preview, err := p.Preview(plans, stripped)
	if err != nil {
		return "", err
	}
	b, _ := json.Marshal(sprigImportArgs{File: file, ID: a.ID, Name: a.Name, Package: base64.StdEncoding.EncodeToString(raw)})
	name := p.Char.DisplayName()
	msg := fmt.Sprintf("📦 Here's everything %s will get from %s. Nothing is written until you say so.\n%s"+
		"Routines run for you. Reply **approve** or **deny**.", name, file, preview)
	t.askAs(t.char, msg, &store.Approval{UserID: t.user.ID, Character: t.char.ID, ChatRef: t.chatRef, Tool: internalPrefix + "import_sprig", Args: string(b)})
	e.logActivity(ctx, "info", "character", t.char.ID, t.user.ID, "proposed importing sprig "+name+" from "+file, "")
	return fmt.Sprintf("PENDING: the user is being shown every file %s will get, as a diff, and asked to approve. Tell them briefly to look it over; don't say it's imported.", name), nil
}

// importSprig imports an approved package, exactly as shown.
func (e *Engine) importSprig(ctx context.Context, user *store.User, args json.RawMessage) (string, error) {
	var a sprigImportArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(a.Package)
	if err != nil {
		return "", err
	}
	p, _, err := e.readPackage(raw, a.ID, a.Name)
	if err != nil {
		return "", err
	}
	res, err := p.Install(ctx, e.DB, e.Chars.Dir(), user.ID, ValidateSkill)
	if err != nil {
		return "", err
	}
	if err := e.Chars.Load(); err != nil {
		e.Log.Warn("reload sprigs after an import", "err", err)
	}
	c := p.Char
	e.trailLevel(ctx, user, c.ID, store.AnyTool, "", store.LevelDraft, "an imported sprig starts at draft")
	e.logActivity(ctx, "info", "character", c.ID, user.ID, fmt.Sprintf("imported sprig %s from %s", c.DisplayName(), a.File), res.Path)
	out := fmt.Sprintf("%s has been imported (%s), with every tool at draft and no memories. They can now be addressed by name.", c.DisplayName(), res.Path)
	if len(res.Routines) > 0 {
		out += fmt.Sprintf(" %d routines scheduled.", len(res.Routines))
	}
	if len(res.Skills) > 0 {
		out += " Skills: " + strings.Join(res.Skills, ", ") + "."
	}
	return out, nil
}
