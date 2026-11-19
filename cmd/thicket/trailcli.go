package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"text/tabwriter"

	"github.com/theshield2594/thicket/internal/config"
)

// `thicket trail` lists the trail straight from the database (it works
// while Thicket is down); `thicket trail undo ID` asks the running server
// to undo an entry, since undoing can mean calling an integration.
func trailCmd(cfg *config.Config, args []string, out io.Writer) error {
	if len(args) > 0 && args[0] == "undo" {
		if len(args) != 2 {
			return errors.New("usage: thicket trail undo ID")
		}
		id, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil || id <= 0 {
			return errors.New("usage: thicket trail undo ID (the number from `thicket trail`)")
		}
		c, err := newClient()
		if err != nil {
			return err
		}
		var resp struct {
			Result string `json:"result"`
		}
		if err := c.do(http.MethodPost, fmt.Sprintf("/api/v1/trail/%d/undo", id), nil, &resp); err != nil {
			return err
		}
		fmt.Fprintln(out, resp.Result)
		return nil
	}
	if len(args) > 0 && args[0] == "list" {
		args = args[1:]
	}
	fs := flag.NewFlagSet("trail", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	limit := fs.Int("limit", 30, "")
	pos, err := parseFlags(fs, args)
	if err != nil || len(pos) != 0 || *limit < 1 {
		return errors.New("usage: thicket trail [list] [--limit N] | thicket trail undo ID")
	}
	db, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	entries, err := db.Trail(context.Background(), 0, *limit)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Fprintln(out, "Nothing in the trail yet.")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "id\twhen\tsprig\tkind\twhat\tundo")
	for _, e := range entries {
		undo := ""
		switch {
		case !e.UndoneAt.IsZero():
			undo = "undone " + e.UndoneAt.Local().Format("Jan 2 15:04")
		case e.Undoable:
			undo = "thicket trail undo " + strconv.FormatInt(e.ID, 10)
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", e.ID, e.At.Local().Format("Jan 2 15:04"), orNone(e.Character), e.Kind, e.Summary, undo)
	}
	return tw.Flush()
}
