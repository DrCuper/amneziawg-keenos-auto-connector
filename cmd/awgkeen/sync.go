package main

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/app"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/domains"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/store"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/syncer"
)

// openSyncer builds app + store + syncer for the stage-2 commands.
func openSyncer(cfgPath string) (*app.App, *store.File, *syncer.Syncer, error) {
	a, err := app.New(cfgPath)
	if err != nil {
		return nil, nil, nil, err
	}
	st, err := a.OpenStore()
	if err != nil {
		_ = a.Close()
		return nil, nil, nil, err
	}
	return a, st, a.NewSyncer(st), nil
}

func printResult(w io.Writer, res *syncer.Result) {
	if res == nil {
		return
	}
	fmt.Fprint(w, res.Summary())
	if res.Apply != nil && res.Apply.Plan != nil {
		fmt.Fprintln(w)
		fmt.Fprint(w, res.Apply.Plan.String())
		switch {
		case res.Apply.DryRun:
			fmt.Fprintln(w, "dry-run: nothing was sent to the router, no versions saved")
		case res.Apply.Applied == 0:
			fmt.Fprintln(w, "router already matches the desired state")
		default:
			fmt.Fprintf(w, "applied %d command(s), saved=%v", res.Apply.Applied, res.Apply.Saved)
			if res.Apply.BackupPath != "" {
				fmt.Fprintf(w, ", backup=%s", res.Apply.BackupPath)
			}
			fmt.Fprintln(w)
		}
	}
}

func newSyncCmd(cfgPath *string) *cobra.Command {
	var opts syncer.Options
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Build domain groups from their sources and apply the diff to the router",
		RunE: func(c *cobra.Command, _ []string) error {
			a, _, s, err := openSyncer(*cfgPath)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close() }()
			res, err := s.Run(c.Context(), opts)
			printResult(c.OutOrStdout(), res)
			return err
		},
	}
	cmd.Flags().BoolVar(&opts.DryRun, "dry-run", false, "build groups and print the plan without changing the router or the state")
	cmd.Flags().BoolVar(&opts.Force, "force", false, "apply even if a group shrank sharply")
	cmd.Flags().StringSliceVarP(&opts.Groups, "group", "g", nil, "only these groups (repeatable)")
	return cmd
}

func newServeCmd(cfgPath *string) *cobra.Command {
	var schedule string
	var noStart bool
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run as a daemon: sync on a cron schedule",
		RunE: func(c *cobra.Command, _ []string) error {
			a, _, s, err := openSyncer(*cfgPath)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close() }()
			if schedule == "" {
				schedule = a.Config.Sync.Schedule
			}
			if _, err := syncer.ParseSchedule(schedule); err != nil {
				return err
			}
			if a.Config.Router.LockFile == "" {
				a.Logger.Warn("router.lock_file is empty: a manual `awgkeen sync` may run concurrently with the daemon")
			}
			runOnStart := a.Config.Sync.RunOnStart == nil || *a.Config.Sync.RunOnStart
			sch := &syncer.Scheduler{Syncer: s, Schedule: schedule, RunOnStart: runOnStart && !noStart, Logger: a.Logger}
			return sch.Start(c.Context())
		},
	}
	cmd.Flags().StringVar(&schedule, "schedule", "", "cron expression (default: sync.schedule from config)")
	cmd.Flags().BoolVar(&noStart, "no-run-on-start", false, "do not sync at startup")
	return cmd
}

func newGroupsCmd(cfgPath *string) *cobra.Command {
	cmd := &cobra.Command{Use: "groups", Short: "Inspect and edit domain groups, their versions and manual entries"}

	list := &cobra.Command{
		Use:   "list",
		Short: "List groups with their latest/applied versions",
		RunE: func(c *cobra.Command, _ []string) error {
			a, st, _, err := openSyncer(*cfgPath)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close() }()
			spec, err := domains.Load(a.DomainsFile())
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(c.OutOrStdout(), 0, 8, 2, ' ', 0)
			fmt.Fprintln(tw, "GROUP\tCONNECTION\tLATEST\tAPPLIED\tENTRIES\tPINNED\tMANUAL\tSOURCES")
			for i := range spec.Groups {
				g := &spec.Groups[i]
				gs, err := st.GroupState(g.Name)
				if err != nil {
					fmt.Fprintf(tw, "%s\t%s\tERROR: %v\n", g.Name, g.Connection, err)
					continue
				}
				latest, _ := st.Latest(g.Name)
				ln, entries := "-", "-"
				if latest != nil {
					ln = fmt.Sprint(latest.Number)
					entries = fmt.Sprint(len(latest.Entries))
				}
				pinned := "-"
				if gs.Pinned > 0 {
					pinned = fmt.Sprint(gs.Pinned)
				}
				manual := fmt.Sprintf("+%d/-%d", len(gs.ManualInclude), len(gs.ManualRemove))
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\n", g.Name, g.Connection, ln, orDash(gs.Applied), entries, pinned, manual, len(g.Sources))
			}
			return tw.Flush()
		},
	}

	var version int
	show := &cobra.Command{
		Use:   "show <group>",
		Short: "Print the entries of a group's version (default: latest)",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			a, st, _, err := openSyncer(*cfgPath)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close() }()
			var v *store.Version
			if version > 0 {
				v, err = st.Version(args[0], version)
			} else {
				v, err = st.Latest(args[0])
				if err == nil && v == nil {
					err = fmt.Errorf("group %s has no versions yet (run `awgkeen sync`)", args[0])
				}
			}
			if err != nil {
				return err
			}
			w := c.OutOrStdout()
			fmt.Fprintf(w, "# group %s version %d (%s), %d entries", args[0], v.Number, v.CreatedAt.Local().Format(time.RFC3339), len(v.Entries))
			if len(v.Exclude) > 0 {
				fmt.Fprintf(w, ", %d exclude", len(v.Exclude))
			}
			fmt.Fprintln(w)
			for _, e := range v.Entries {
				fmt.Fprintln(w, e)
			}
			for _, e := range v.Exclude {
				fmt.Fprintln(w, "!"+e)
			}
			return nil
		},
	}
	show.Flags().IntVar(&version, "version", 0, "version number (default: latest)")

	history := &cobra.Command{
		Use:   "history <group>",
		Short: "List stored versions of a group",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			a, st, _, err := openSyncer(*cfgPath)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close() }()
			gs, err := st.GroupState(args[0])
			if err != nil {
				return err
			}
			vs, err := st.Versions(args[0])
			if err != nil {
				return err
			}
			if len(vs) == 0 {
				fmt.Fprintf(c.OutOrStdout(), "group %s has no versions yet\n", args[0])
				return nil
			}
			tw := tabwriter.NewWriter(c.OutOrStdout(), 0, 8, 2, ' ', 0)
			fmt.Fprintln(tw, "VERSION\tCREATED\tENTRIES\tDIFF\tREASON\tFLAGS")
			var prev *store.Version
			for _, v := range vs {
				diff := "-"
				if prev != nil {
					add, rm := domains.Diff(prev.Entries, v.Entries)
					diff = fmt.Sprintf("+%d/-%d", len(add), len(rm))
				}
				var flags []string
				if v.Number == gs.Applied {
					flags = append(flags, "applied")
				}
				if v.Number == gs.Pinned {
					flags = append(flags, "pinned")
				}
				if v.Truncated > 0 {
					flags = append(flags, fmt.Sprintf("truncated:%d", v.Truncated))
				}
				for _, s := range v.Sources {
					if s.Stale {
						flags = append(flags, "stale:"+s.Name)
					}
				}
				fmt.Fprintf(tw, "%d\t%s\t%d\t%s\t%s\t%s\n", v.Number, v.CreatedAt.Local().Format("2006-01-02 15:04:05"), len(v.Entries), diff, v.Reason, strings.Join(flags, ","))
				prev = v
			}
			return tw.Flush()
		},
	}

	var apply, dryRun, force bool
	addFlags := func(c *cobra.Command) {
		c.Flags().BoolVar(&apply, "apply", false, "run sync for the group right away")
		c.Flags().BoolVar(&dryRun, "dry-run", false, "with --apply: print the plan only")
	}
	manual := func(use, short string, fn func(*syncer.Syncer, string, []string) ([]string, error)) *cobra.Command {
		c := &cobra.Command{
			Use: use, Short: short, Args: cobra.MinimumNArgs(2),
			RunE: func(c *cobra.Command, args []string) error {
				a, _, s, err := openSyncer(*cfgPath)
				if err != nil {
					return err
				}
				defer func() { _ = a.Close() }()
				if err := requireGroup(a, args[0]); err != nil {
					return err
				}
				norm, err := fn(s, args[0], args[1:])
				if err != nil {
					return err
				}
				fmt.Fprintf(c.OutOrStdout(), "group %s: recorded %s\n", args[0], strings.Join(norm, ", "))
				if !apply {
					fmt.Fprintln(c.OutOrStdout(), "run `awgkeen sync` (or --apply) to push it to the router")
					return nil
				}
				res, err := s.Run(c.Context(), syncer.Options{Groups: []string{args[0]}, DryRun: dryRun, Force: force})
				printResult(c.OutOrStdout(), res)
				return err
			},
		}
		addFlags(c)
		c.Flags().BoolVar(&force, "force", false, "with --apply: bypass the shrink guard")
		return c
	}
	add := manual("add <group> <entry>...", "Always include entries in a group (stored in state, no YAML edit)", (*syncer.Syncer).AddManual)
	remove := manual("remove <group> <entry>...", "Always drop entries from a group (stored in state, never sent to the router)", (*syncer.Syncer).RemoveManual)

	clear := &cobra.Command{
		Use:   "clear <group>",
		Short: "Forget manual add/remove entries of a group",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			a, _, s, err := openSyncer(*cfgPath)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close() }()
			if err := s.ClearManual(args[0]); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "group %s: manual entries cleared\n", args[0])
			return nil
		},
	}

	var to int
	var rbDry bool
	rollback := &cobra.Command{
		Use:   "rollback <group>",
		Short: "Pin a group to a previous version and apply it (default: the version before the applied one)",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			a, _, s, err := openSyncer(*cfgPath)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close() }()
			res, err := s.Rollback(c.Context(), args[0], to, rbDry)
			printResult(c.OutOrStdout(), res)
			if err != nil {
				return err
			}
			if !rbDry {
				fmt.Fprintf(c.OutOrStdout(), "group %s is pinned; run `awgkeen groups unpin %s` to resume updates from sources\n", args[0], args[0])
			}
			return nil
		},
	}
	rollback.Flags().IntVar(&to, "to", 0, "version number to roll back to")
	rollback.Flags().BoolVar(&rbDry, "dry-run", false, "print the plan only; do not pin")

	unpin := &cobra.Command{
		Use:   "unpin <group>",
		Short: "Resume building the group from its sources",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			a, _, s, err := openSyncer(*cfgPath)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close() }()
			if err := s.Unpin(args[0]); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "group %s unpinned; next sync builds it from sources\n", args[0])
			return nil
		},
	}

	cmd.AddCommand(list, show, history, add, remove, clear, rollback, unpin)
	return cmd
}

func requireGroup(a *app.App, name string) error {
	spec, err := domains.Load(a.DomainsFile())
	if err != nil {
		return err
	}
	if _, ok := spec.Group(name); !ok {
		return fmt.Errorf("group %q is not defined in %s", name, a.DomainsFile())
	}
	return nil
}

func orDash(n int) string {
	if n == 0 {
		return "-"
	}
	return fmt.Sprint(n)
}
