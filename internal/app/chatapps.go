package app

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/theshield2594/thicket/internal/channel"
)

// liveChan is a chat adapter that is running.
type liveChan struct {
	ch     channel.Channel
	token  string // "" when the app didn't build it (tests)
	cancel context.CancelFunc
	done   chan struct{}
	err    error       // Run's result, set before done closes
	kept   atomic.Bool // a chat connection that came up and is in use
}

// chanList is the chat adapters right now (they can change while Thicket
// runs: see ConnectChat).
func (a *App) chanList() []channel.Channel {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]channel.Channel(nil), a.channels...)
}

// launch runs a chat adapter until Thicket stops or its cancel is called.
// onExit, if set, gets Run's error once it has returned.
func (a *App) launch(ch channel.Channel, token string, kept bool, onExit func(lc *liveChan)) (*liveChan, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.runCtx == nil || a.stopping {
		return nil, errors.New("Thicket isn't running yet; try again in a moment")
	}
	ctx, cancel := context.WithCancel(a.runCtx)
	lc := &liveChan{ch: ch, token: token, cancel: cancel, done: make(chan struct{})}
	lc.kept.Store(kept)
	a.goRun("channel "+ch.Name(), func() {
		defer close(lc.done)
		defer cancel()
		lc.err = ch.Run(ctx, a.Engine.HandleIncoming)
		if onExit != nil {
			onExit(lc)
		}
	})
	return lc, nil
}

// chatConnectTimeout is how long a chat app gets to come online.
const chatConnectTimeout = 40 * time.Second

// ConnectChat connects a chat app ("discord", "telegram") with a bot token
// while Thicket runs, replacing the connection it has. It returns once the
// bot is online. A token that doesn't work leaves things as they were.
// Storing the token is the caller's job.
func (a *App) ConnectChat(ctx context.Context, name, token string) error {
	if a.newChannel == nil {
		return errors.New("this Thicket can't connect chat apps while it runs")
	}
	a.connectMu.Lock()
	defer a.connectMu.Unlock()

	a.mu.Lock()
	old := a.live[name]
	a.mu.Unlock()
	if old != nil && old.token == token && old.ch.Connected() {
		return nil
	}
	ch := a.newChannel(name, token, a.Log)
	if ch == nil && name == "matrix" {
		return errors.New("Matrix needs THICKET_MATRIX_HOMESERVER set to the homeserver's address (such as https://matrix.example.org) first; set it, restart Thicket, then send the access token again")
	}
	if ch == nil {
		return fmt.Errorf("Thicket has no chat app called %q (discord and telegram are the ones that take a bot token)", name)
	}
	if c, ok := ch.(channel.Checker); ok {
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := c.Check(cctx)
		cancel()
		if err != nil {
			return err
		}
	}
	// One bot per app: the old connection goes before the new one comes up
	// (two Telegram pollers for one bot knock each other out).
	if old != nil {
		old.kept.Store(false)
		old.cancel()
		<-old.done
	}
	lc, err := a.bringUp(ctx, ch, token)
	if err != nil {
		a.dropChat(name, old)
		if old != nil && old.token != "" {
			// Put back what worked before.
			if prev := a.newChannel(name, old.token, a.Log); prev != nil {
				if plc, perr := a.bringUp(ctx, prev, old.token); perr == nil {
					a.keepChat(name, plc)
				} else {
					a.Log.Error("couldn't restore the previous chat connection", "app", name, "err", perr)
				}
			}
		}
		return err
	}
	a.keepChat(name, lc)
	a.Log.Info("chat app connected while running", "app", name)
	return nil
}

// bringUp starts an adapter and waits until it is online.
func (a *App) bringUp(ctx context.Context, ch channel.Channel, token string) (*liveChan, error) {
	a.Engine.AddChannel(ch) // before Run: a first message needs somewhere to answer
	lc, err := a.launch(ch, token, false, a.chatExited)
	if err != nil {
		a.Engine.RemoveChannel(ch)
		return nil, err
	}
	deadline := time.NewTimer(chatConnectTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for !ch.Connected() {
		err = nil
		select {
		case <-lc.done:
			if err = lc.err; err == nil {
				err = errors.New("the connection closed straight away")
			}
		case <-ctx.Done():
			err = ctx.Err()
		case <-deadline.C:
			err = fmt.Errorf("%s didn't come online within %s", ch.Name(), chatConnectTimeout)
		case <-tick.C:
			continue
		}
		lc.cancel()
		<-lc.done
		a.Engine.RemoveChannel(ch)
		return nil, err
	}
	return lc, nil
}

// keepChat records a connection that came up, in place of the app's old one.
func (a *App) keepChat(name string, lc *liveChan) {
	lc.kept.Store(true)
	a.mu.Lock()
	defer a.mu.Unlock()
	out := a.channels[:0:0]
	for _, c := range a.channels {
		if c.Name() != name {
			out = append(out, c)
		}
	}
	a.channels = append(out, lc.ch)
	a.live[name] = lc
}

// dropChat forgets an app's connection, if it is still lc.
func (a *App) dropChat(name string, lc *liveChan) {
	if lc == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.live[name] != lc {
		return
	}
	delete(a.live, name)
	out := a.channels[:0:0]
	for _, c := range a.channels {
		if c != lc.ch {
			out = append(out, c)
		}
	}
	a.channels = out
}

// chatExited runs when a chat app connected in chat stops. If it was in
// use and Thicket isn't shutting down, the owner hears about it; Thicket
// itself keeps running.
func (a *App) chatExited(lc *liveChan) {
	if !lc.kept.Load() || lc.err == nil {
		return
	}
	a.mu.Lock()
	ctx, stopping := a.runCtx, a.stopping
	a.mu.Unlock()
	if stopping || ctx.Err() != nil {
		return
	}
	name := lc.ch.Name()
	a.Engine.RemoveChannel(lc.ch)
	a.dropChat(name, lc)
	a.Log.Error("chat app stopped", "app", name, "err", lc.err)
	a.Engine.Notify(ctx, "chat-stopped:"+name, time.Hour, "error",
		fmt.Sprintf("I lost the %s connection (%s). Send me the bot token again to reconnect.", name, shortErr(lc.err)))
}
