package agent

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/theshield2594/thicket/internal/secrets"
	"github.com/theshield2594/thicket/internal/settings"
	"github.com/theshield2594/thicket/internal/store"
)

// The web app's Settings screen. It does what the host's settings and
// integration tools do in chat, for the owner and admins, and logs the
// same activity.

// SettingInfo is one setting as the Settings screen shows it.
type SettingInfo struct {
	Name    string   `json:"name"`
	Help    string   `json:"help"`
	Kind    string   `json:"kind"`
	Value   string   `json:"value"`
	Default string   `json:"default"`
	Changed bool     `json:"changed"`
	Choices []string `json:"choices,omitempty"`
}

// IntegrationInfo is one connector (MCP server) and how it's doing.
type IntegrationInfo struct {
	Name      string   `json:"name"`
	Transport string   `json:"transport"`
	Target    string   `json:"target"`
	Status    string   `json:"status"`
	Tools     []string `json:"tools"`
	UsedBy    []string `json:"used_by"`
}

func (e *Engine) settingInfo(ctx context.Context, k settings.Key) SettingInfo {
	return SettingInfo{Name: k.Name, Help: k.Help, Kind: k.Kind, Value: e.Settings.String(ctx, k.Name),
		Default: e.Settings.Default(k.Name), Changed: e.Settings.IsSet(ctx, k.Name), Choices: settings.Choices(k.Name)}
}

// AdminSettings lists every setting, in the order they're declared.
func (e *Engine) AdminSettings(ctx context.Context) any {
	out := make([]SettingInfo, 0, len(settings.Known))
	for _, k := range settings.Known {
		out = append(out, e.settingInfo(ctx, k))
	}
	return out
}

// AdminSetSetting changes a setting; an empty value puts the default back.
func (e *Engine) AdminSetSetting(ctx context.Context, by *store.User, name, value string) (any, error) {
	if !by.IsAdmin() {
		return nil, errors.New("only the owner or an admin can change settings")
	}
	for _, k := range settings.Known {
		if k.Name != name {
			continue
		}
		if err := e.Settings.Set(ctx, name, value); err != nil {
			return nil, err
		}
		e.logActivity(ctx, "info", "system", "", by.ID, "setting changed: "+name, e.Settings.String(ctx, name))
		return e.settingInfo(ctx, k), nil
	}
	return nil, fmt.Errorf("unknown setting %q", name)
}

func (e *Engine) integrationInfo(s store.MCPServer) IntegrationInfo {
	in := IntegrationInfo{Name: s.Name, Transport: s.Transport, Target: s.Target, Status: e.MCP.Status()[s.Name], Tools: []string{}, UsedBy: []string{}}
	if in.Status == "" {
		in.Status = "not connected"
	}
	for _, t := range e.MCP.Tools() {
		if t.Server == s.Name {
			in.Tools = append(in.Tools, t.Tool.Name)
		}
	}
	sort.Strings(in.Tools)
	for _, c := range e.Chars.All() {
		for _, sc := range c.Tools {
			if strings.HasPrefix(sc, "mcp:"+s.Name+"/") {
				in.UsedBy = append(in.UsedBy, c.DisplayName())
				break
			}
		}
	}
	return in
}

// AdminIntegrations lists the connectors and their status.
func (e *Engine) AdminIntegrations(ctx context.Context) (any, error) {
	servers, err := e.DB.MCPServers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]IntegrationInfo, 0, len(servers))
	for _, s := range servers {
		out = append(out, e.integrationInfo(s))
	}
	return out, nil
}

// AdminAddIntegration connects an MCP server over HTTP. A token, if given,
// is stored encrypted as the secret NAME_token and sent as a bearer
// header; it is never stored with the server or logged. Servers you sign
// in to (OAuth) are still connected by asking the host in chat.
func (e *Engine) AdminAddIntegration(ctx context.Context, by *store.User, name, rawURL, token string) (any, error) {
	if !by.IsAdmin() {
		return nil, errors.New("only the owner or an admin can connect things")
	}
	name = strings.ToLower(strings.TrimSpace(name))
	if !secrets.ValidName(name) || strings.Contains(name, "__") {
		return nil, errors.New("the name needs 2 to 48 lowercase letters, digits or single underscores, e.g. home_assistant")
	}
	rawURL = strings.TrimSpace(rawURL)
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return nil, errors.New("the address needs to be an http(s) URL, e.g. http://homeassistant.local:8123/api/mcp")
	}
	servers, err := e.DB.MCPServers(ctx)
	if err != nil {
		return nil, err
	}
	for _, s := range servers {
		if s.Name == name {
			return nil, fmt.Errorf("there's already a connector called %s; remove it first or pick another name", name)
		}
	}
	s := store.MCPServer{Name: name, Transport: "http", Target: rawURL, Enabled: true}
	token = strings.TrimSpace(token)
	secret := ""
	if token != "" {
		secret = name + "_token"
		if !secrets.ValidName(secret) {
			return nil, errors.New("that name is too long to keep a token under; pick a shorter one")
		}
		// Never replace a secret this call didn't make: it may be someone
		// else's credential, and undoing a failed attempt would delete it.
		if exists, err := e.Secrets.Stored(ctx, secret); err != nil {
			return nil, err
		} else if exists {
			return nil, fmt.Errorf("a secret called %s is already stored; pick another name for the connector", secret)
		}
		if err := e.Secrets.Put(ctx, secret, token); err != nil {
			return nil, err
		}
		s.Headers = map[string]string{"Authorization": "Bearer {{secret:" + secret + "}}"}
	}
	// undo removes what this call stored and says what it couldn't remove,
	// so "nothing was saved" is never claimed falsely. It runs even if the
	// request was cancelled meanwhile (a closed tab, a timeout).
	undo := func(cause string) error {
		bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		var left []string
		e.MCP.Disconnect(name)
		if err := e.DB.DeleteMCPServer(bg, name); err != nil {
			e.Log.Error("undo connector", "name", name, "err", err)
			left = append(left, "the connector "+name+" (remove it from the list)")
		}
		if secret != "" {
			if err := e.Secrets.Delete(bg, secret); err != nil {
				e.Log.Error("undo connector token", "secret", secret, "err", err)
				left = append(left, "its token, the secret "+secret)
			}
		}
		if len(left) > 0 {
			return fmt.Errorf("%s. Undoing it failed too, so this is still stored: %s", cause, strings.Join(left, "; "))
		}
		return fmt.Errorf("%s, so nothing was saved", cause)
	}
	if err := e.DB.PutMCPServer(ctx, s); err != nil {
		e.Log.Error("save connector", "name", name, "err", err)
		return nil, undo("couldn't save the connector")
	}
	tools, err := e.MCP.Connect(ctx, s)
	if err != nil {
		e.logActivity(context.WithoutCancel(ctx), "warn", "tool", "", by.ID, "integration "+name+" failed to connect", err.Error())
		return nil, undo("couldn't connect (" + e.Secrets.Redact(err.Error()) + ")")
	}
	e.logActivity(ctx, "info", "tool", "", by.ID, "integration "+name+" connected", fmt.Sprintf("%d tools", len(tools)))
	return e.integrationInfo(s), nil
}

// AdminRemoveIntegration disconnects a connector and forgets it.
func (e *Engine) AdminRemoveIntegration(ctx context.Context, by *store.User, name string) error {
	if !by.IsAdmin() {
		return errors.New("only the owner or an admin can remove connectors")
	}
	e.MCP.Disconnect(name)
	if err := e.DB.DeleteMCPServer(ctx, name); err != nil {
		return err
	}
	if e.OAuth != nil {
		_ = e.OAuth.Forget(ctx, name)
	}
	e.logActivity(ctx, "info", "tool", "", by.ID, "integration "+name+" removed", "")
	return nil
}
