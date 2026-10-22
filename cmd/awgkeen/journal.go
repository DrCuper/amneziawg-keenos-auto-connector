package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/app"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/health"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/journal"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/notify"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router"
)

func openJournal(cfgPath string) (*app.App, *journal.Journal, error) {
	a, err := app.New(cfgPath)
	if err != nil {
		return nil, nil, err
	}
	j := a.Journal()
	if j == nil {
		_ = a.Close()
		return nil, nil, errors.New("journal is disabled (journal.enabled: false) or cannot be opened")
	}
	return a, j, nil
}

func newJournalCmd(cfgPath *string) *cobra.Command {
	cmd := &cobra.Command{Use: "journal", Short: "Show the history of syncs, rollbacks and manual edits"}

	var f journal.Filter
	var since string
	var real, dry, asJSON bool
	list := &cobra.Command{
		Use:   "list",
		Short: "List journal entries, newest first",
		RunE: func(c *cobra.Command, _ []string) error {
			if real && dry {
				return errors.New("--real and --dry-run are mutually exclusive")
			}
			if real || dry {
				f.DryRun = &dry
			}
			if since != "" {
				t, err := parseSince(since, time.Now())
				if err != nil {
					return err
				}
				f.Since = t
			}
			a, j, err := openJournal(*cfgPath)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close() }()
			es, err := j.Query(f)
			if err != nil {
				return err
			}
			w := c.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(w)
				for _, e := range es {
					if err := enc.Encode(e); err != nil {
						return err
					}
				}
				return nil
			}
			if len(es) == 0 {
				fmt.Fprintln(w, "no journal entries")
				return nil
			}
			return printEntries(w, es)
		},
	}
	list.Flags().StringVarP(&f.Group, "group", "g", "", "only entries touching this group")
	list.Flags().StringVar(&f.Kind, "kind", "", "sync | rollback | manual | router-apply")
	list.Flags().StringVar(&f.Status, "status", "", "ok | partial | failed | skipped")
	list.Flags().StringVar(&since, "since", "", "only newer entries: duration (24h, 7d) or date (2026-10-20, RFC 3339)")
	list.Flags().BoolVar(&real, "real", false, "only real runs (no dry-run)")
	list.Flags().BoolVar(&dry, "dry-run", false, "only dry-runs")
	list.Flags().IntVarP(&f.Limit, "limit", "n", 20, "show at most N entries (0 = all)")
	list.Flags().BoolVar(&asJSON, "json", false, "print JSON lines")

	var showJSON bool
	show := &cobra.Command{
		Use:   "show <id>",
		Short: "Print one journal entry in full, with its diff",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("invalid id %q", args[0])
			}
			a, j, err := openJournal(*cfgPath)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close() }()
			e, err := j.Get(id)
			if err != nil {
				return err
			}
			if showJSON {
				enc := json.NewEncoder(c.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(e)
			}
			printEntry(c.OutOrStdout(), e)
			return nil
		},
	}
	show.Flags().BoolVar(&showJSON, "json", false, "print JSON")

	prune := &cobra.Command{
		Use:   "prune",
		Short: "Drop entries older than journal.keep_days and beyond journal.max_entries",
		RunE: func(c *cobra.Command, _ []string) error {
			a, j, err := openJournal(*cfgPath)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close() }()
			n, err := j.Prune()
			if err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "removed %d entr%s\n", n, map[bool]string{true: "y", false: "ies"}[n == 1])
			return nil
		},
	}

	cmd.AddCommand(list, show, prune)
	return cmd
}

// parseSince accepts a duration (90m, 24h, 7d) or a date/time.
func parseSince(s string, now time.Time) (time.Time, error) {
	t, err := journal.ParseSince(s, now)
	if err != nil {
		return t, fmt.Errorf("--since %w", err)
	}
	return t, nil
}

func mode(e *journal.Entry) string {
	if e.DryRun {
		return "dry-run"
	}
	return "real"
}

func printEntries(w io.Writer, es []*journal.Entry) error {
	tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTIME\tKIND\tTRIGGER\tMODE\tSTATUS\tGROUPS\tSUMMARY")
	for _, e := range es {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", e.ID, e.Time.Local().Format("2006-01-02 15:04:05"), e.Kind, e.Trigger, mode(e), e.Status, groupsSummary(e), entrySummary(e))
	}
	return tw.Flush()
}

func groupsSummary(e *journal.Entry) string {
	if e.Manual != nil {
		return e.Manual.Group
	}
	var parts []string
	for _, g := range e.Groups {
		if g.Error != "" {
			parts = append(parts, g.Name+" ERR")
			continue
		}
		p := fmt.Sprintf("%s v%d", g.Name, g.Version)
		if g.Added > 0 || g.Removed > 0 {
			p += fmt.Sprintf(" +%d/-%d", g.Added, g.Removed)
		}
		parts = append(parts, p)
	}
	if len(parts) == 0 {
		groups := map[string]bool{}
		for _, c := range e.Changes {
			groups[c.Group] = true
		}
		for g := range groups {
			parts = append(parts, g)
		}
		sort.Strings(parts)
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ", ")
}

func entrySummary(e *journal.Entry) string {
	var s string
	switch {
	case e.Manual != nil:
		s = e.Manual.Op
		if len(e.Manual.Entries) > 0 {
			s += " " + strings.Join(e.Manual.Entries, " ")
		}
	case e.Error != "" && e.Status != journal.StatusPartial:
		s = e.Error
	case e.DryRun && e.Commands > 0:
		s = fmt.Sprintf("%d command(s) planned", e.Commands)
	case e.Applied > 0:
		s = fmt.Sprintf("%d command(s) applied", e.Applied)
		if e.Saved {
			s += ", saved"
		}
	default:
		s = "no changes"
	}
	if e.Status == journal.StatusPartial {
		s += " (some groups failed)"
	}
	if r := []rune(s); len(r) > 80 {
		s = string(r[:79]) + "…"
	}
	return s
}

func printEntry(w io.Writer, e *journal.Entry) {
	fmt.Fprintf(w, "entry %d: %s %s, %s, trigger %s\n", e.ID, mode(e), e.Kind, e.Status, e.Trigger)
	fmt.Fprintf(w, "time: %s, took %s\n", e.Time.Local().Format(time.RFC3339), (time.Duration(e.DurationMS) * time.Millisecond).String())
	if e.Error != "" {
		fmt.Fprintf(w, "error: %s\n", e.Error)
	}
	if m := e.Manual; m != nil {
		fmt.Fprintf(w, "manual %s on group %s", m.Op, m.Group)
		if len(m.Entries) > 0 {
			fmt.Fprintf(w, ": %s", strings.Join(m.Entries, ", "))
		}
		fmt.Fprintln(w)
	}
	for _, g := range e.Groups {
		if g.Error != "" {
			fmt.Fprintf(w, "group %s: ERROR: %s\n", g.Name, g.Error)
			continue
		}
		fmt.Fprintf(w, "group %s -> %s: version %d", g.Name, g.Connection, g.Version)
		switch {
		case g.Pinned:
			fmt.Fprint(w, " (pinned)")
		case g.NewVersion:
			fmt.Fprint(w, " (new)")
		}
		fmt.Fprintf(w, ", %d entries", g.Entries)
		if g.PrevApplied > 0 {
			fmt.Fprintf(w, ", +%d/-%d vs applied v%d", g.Added, g.Removed, g.PrevApplied)
		}
		fmt.Fprintln(w)
	}
	if e.Commands > 0 || e.Applied > 0 {
		fmt.Fprintf(w, "commands: %d planned, %d applied, saved=%v", e.Commands, e.Applied, e.Saved)
		if e.Backup != "" {
			fmt.Fprintf(w, ", backup=%s", e.Backup)
		}
		fmt.Fprintln(w)
	}
	if len(e.Changes) > 0 {
		fmt.Fprintln(w, "\ndiff:")
		cur := ""
		for _, c := range e.Changes {
			if key := c.Kind + "|" + c.Group; key != cur {
				cur = key
				if c.Kind == router.KindInterface {
					fmt.Fprintf(w, "interface %s:\n", c.Group)
				} else {
					fmt.Fprintf(w, "group %s:\n", c.Group)
				}
			}
			fmt.Fprintf(w, "  %s\n", c.Text)
		}
		if e.ChangesOmitted > 0 {
			fmt.Fprintf(w, "  ... %d more line(s) not stored (journal.max_changes)\n", e.ChangesOmitted)
		}
	}
}

func newNotifyCmd(cfgPath *string) *cobra.Command {
	cmd := &cobra.Command{Use: "notify", Short: "Check notification channels and active alerts"}

	var text string
	test := &cobra.Command{
		Use:   "test",
		Short: "Send a test message to every configured channel",
		RunE: func(c *cobra.Command, _ []string) error {
			a, err := app.New(*cfgPath)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close() }()
			al := a.Alerter()
			if len(al.Channels) == 0 {
				return errors.New("no channels configured (notify.telegram, notify.webhooks)")
			}
			res := al.Send(c.Context(), &notify.Message{Event: notify.EventTest, Title: "awgkeen test message", Text: text})
			names := make([]string, 0, len(res))
			for n := range res {
				names = append(names, n)
			}
			sort.Strings(names)
			failed := 0
			for _, n := range names {
				if err := res[n]; err != nil {
					failed++
					fmt.Fprintf(c.OutOrStdout(), "%s: FAILED: %v\n", n, err)
				} else {
					fmt.Fprintf(c.OutOrStdout(), "%s: ok\n", n)
				}
			}
			if failed > 0 {
				return fmt.Errorf("%d of %d channel(s) failed", failed, len(names))
			}
			return nil
		},
	}
	test.Flags().StringVar(&text, "message", "If you can read this, notifications work.", "message text")

	status := &cobra.Command{
		Use:   "status",
		Short: "List active and pending alerts",
		RunE: func(c *cobra.Command, _ []string) error {
			a, err := app.New(*cfgPath)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close() }()
			al := a.Alerter()
			st, err := al.States()
			if err != nil {
				return err
			}
			w := c.OutOrStdout()
			var names []string
			for _, ch := range al.Channels {
				names = append(names, ch.Name())
			}
			if len(names) == 0 {
				names = []string{"none"}
			}
			fmt.Fprintf(w, "channels: %s\n", strings.Join(names, ", "))
			if len(st) == 0 {
				fmt.Fprintln(w, "no active alerts")
				return nil
			}
			tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
			fmt.Fprintln(tw, "ALERT\tSTATE\tSINCE\tFAILURES\tLAST SENT\tDETAIL")
			for _, s := range st {
				key := s.Alert
				if s.Check != "" {
					key += ":" + s.Check
				}
				state := "pending"
				if s.Active {
					state = "firing"
					if !s.Notified {
						state = "firing (undelivered)"
					}
				}
				sent := "-"
				if !s.LastSent.IsZero() {
					sent = s.LastSent.Local().Format("2006-01-02 15:04")
				}
				detail := s.Detail
				if r := []rune(detail); len(r) > 80 {
					detail = string(r[:79]) + "…"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\n", key, state, s.Since.Local().Format("2006-01-02 15:04"), s.Failures, sent, detail)
			}
			return tw.Flush()
		},
	}

	cmd.AddCommand(test, status)
	return cmd
}

func newHealthCmd(cfgPath *string) *cobra.Command {
	var update bool
	cmd := &cobra.Command{
		Use:   "health",
		Short: "Run the health checks once (router reachability, AWG tunnels, health.checks hooks)",
		RunE: func(c *cobra.Command, _ []string) error {
			a, err := app.New(*cfgPath)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close() }()
			checks := a.HealthChecks()
			if len(checks) == 0 {
				fmt.Fprintln(c.OutOrStdout(), "no checks configured")
				return nil
			}
			res := health.RunAll(c.Context(), checks)
			failed := 0
			var signals []notify.Signal
			for _, r := range res {
				signals = append(signals, r.Signal)
				if r.Signal.Err != nil {
					failed++
					fmt.Fprintf(c.OutOrStdout(), "%s: FAIL (%s): %v\n", r.Signal.Key(), r.Took.Round(time.Millisecond), r.Signal.Err)
				} else {
					fmt.Fprintf(c.OutOrStdout(), "%s: ok (%s)\n", r.Signal.Key(), r.Took.Round(time.Millisecond))
				}
			}
			if update {
				a.Alerter().Update(c.Context(), signals...)
			}
			if failed > 0 {
				return fmt.Errorf("%d check(s) failed", failed)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&update, "notify", false, "also update alerts (send firing/resolved like the daemon)")
	return cmd
}
