package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/theshield2594/thicket/internal/mcp"
	"github.com/theshield2594/thicket/internal/secrets"
	"github.com/theshield2594/thicket/internal/store"
)

// addOAuthIntegration saves an integration that signs in with OAuth and
// starts the sign-in. The link is posted by Thicket after the reply, so
// the model never handles it; when the person finishes, the integration
// connects and the host picks the conversation back up.
func (e *Engine) addOAuthIntegration(ctx context.Context, t *turn, a store.MCPServer, clientID, clientSecret string) (string, error) {
	if e.OAuth == nil {
		return "", errors.New("OAuth sign-in isn't available")
	}
	if a.Transport != "http" {
		return "", errors.New("OAuth needs transport http")
	}
	if len(mcp.OAuthSecretName(a.Name)) > 48 {
		return "", errors.New("that name is too long for an OAuth integration (at most 42 characters)")
	}
	for k := range a.Headers {
		if strings.EqualFold(k, "Authorization") {
			return "", errors.New("leave out the Authorization header: OAuth supplies it")
		}
	}
	if secrets.LooksLikeSecret(clientSecret) || (clientSecret != "" && !strings.HasPrefix(clientSecret, "{{secret:")) {
		return "", errors.New("client_secret must be a {{secret:NAME}} placeholder; use request_secret first")
	}
	secret, err := e.Secrets.Expand(ctx, clientSecret)
	if err != nil {
		return "", err
	}
	if a.Headers == nil {
		a.Headers = map[string]string{}
	}
	a.Headers["Authorization"] = mcp.OAuthHeader
	a.Enabled = true
	if err := e.DB.PutMCPServer(ctx, a); err != nil {
		return "", err
	}
	user, chatRef, server := t.user, t.chatRef, a
	link, err := e.OAuth.Start(ctx, a.Name, a.Target, clientID, secret, func(ctx context.Context, err error) {
		e.oauthDone(ctx, user, chatRef, server, err)
	})
	if err != nil {
		e.logActivity(ctx, "warn", "tool", t.char.ID, t.user.ID, "sign-in for integration "+a.Name+" couldn't start", err.Error())
		return "", fmt.Errorf("saved, but sign-in couldn't start: %v", err)
	}
	note := ""
	if e.Cfg.PublicURL == "" {
		note = "\n(Afterwards your browser comes back to " + e.Cfg.WebBaseURL() + ", which only works on the machine running Thicket. Set THICKET_PUBLIC_URL to sign in from anywhere.)"
	}
	t.afterReply = append(t.afterReply, post{text: fmt.Sprintf("Sign in to **%s** here: %s\nThe link works once, for 15 minutes.%s", a.Name, link, note)})
	e.logActivity(ctx, "info", "tool", t.char.ID, t.user.ID, "sign-in for integration "+a.Name+" started", "")
	return fmt.Sprintf("Saved %s. Thicket will post the user a sign-in link right after your reply (don't write a link yourself). "+
		"When they've signed in, the integration connects and you'll hear about it; then ask who should use its tools.", a.Name), nil
}

// oauthDone runs when the person comes back from signing in.
func (e *Engine) oauthDone(ctx context.Context, user *store.User, chatRef string, s store.MCPServer, signInErr error) {
	host, ok := e.Chars.Host()
	if !ok {
		return
	}
	if signInErr != nil {
		e.logActivity(ctx, "warn", "tool", host.ID, user.ID, "sign-in for integration "+s.Name+" failed", signInErr.Error())
		_ = e.send(ctx, chatRef, host, fmt.Sprintf("Signing in to %s didn't work (%s). Ask me for a new link to try again.", s.Name, shortErr(signInErr)))
		return
	}
	event := ""
	tools, err := e.MCP.Connect(ctx, s)
	if err != nil {
		e.logActivity(ctx, "warn", "tool", host.ID, user.ID, "integration "+s.Name+" failed to connect after sign-in", err.Error())
		event = fmt.Sprintf("The user signed in to the %s integration, but connecting failed: %s. Tell them, and suggest trying again later.", s.Name, shortErr(err))
	} else {
		var names []string
		for _, tl := range tools {
			names = append(names, tl.Name)
		}
		e.logActivity(ctx, "info", "tool", host.ID, user.ID, "integration "+s.Name+" connected after sign-in", strings.Join(names, ", "))
		event = fmt.Sprintf("The user just signed in to %s, and it's connected with %d tools: %s. No sprig can use them yet: ask the user who should, then grant_tools with mcp:%s/* or specific tools.",
			s.Name, len(tools), strings.Join(names, ", "), s.Name)
	}
	unlock := e.lockChat(chatRef)
	defer unlock()
	conv, err := e.DB.OpenConversation(ctx, user.ID, chatRef, e.Cfg.IdleAfter)
	if err != nil {
		e.Log.Error("open conversation after sign-in", "err", err)
		return
	}
	_ = e.DB.AddMessage(ctx, conv.ID, "event", "", event)
	e.active.Store(chatRef, host.ID)
	if err := e.respond(ctx, &turn{user: user, char: host, chatRef: chatRef, conv: conv, isDM: true}, ""); err != nil {
		e.Log.Error("reply after sign-in", "err", err)
	}
}
