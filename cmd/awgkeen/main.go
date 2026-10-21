// Command awgkeen manages domain-based routing and AmneziaWG connections on
// Keenetic routers: `router` (stage 1), `sync`, `serve` and `groups` (stage 2),
// `journal`, `notify` and `health` (stage 3).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/app"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/journal"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := newRoot().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(exitCode(err))
	}
}

func newRoot() *cobra.Command {
	var cfgPath string
	root := &cobra.Command{
		Use:           "awgkeen",
		Short:         "AmneziaWG + Keenetic domain routing manager",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVarP(&cfgPath, "config", "c", "config.yaml", "path to YAML config")
	root.AddCommand(newRouterCmd(&cfgPath), newSyncCmd(&cfgPath), newServeCmd(&cfgPath), newGroupsCmd(&cfgPath),
		newJournalCmd(&cfgPath), newNotifyCmd(&cfgPath), newHealthCmd(&cfgPath))
	return root
}

func newRouterCmd(cfgPath *string) *cobra.Command {
	cmd := &cobra.Command{Use: "router", Short: "Read and change the Keenetic router configuration"}

	var raw bool
	show := &cobra.Command{
		Use:   "show",
		Short: "Show FQDN groups and dns-proxy routes currently on the router",
		RunE: func(c *cobra.Command, _ []string) error {
			a, err := app.New(*cfgPath)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close() }()
			st, err := a.Client.ReadState(c.Context())
			if err != nil {
				return err
			}
			if raw {
				fmt.Fprint(c.OutOrStdout(), st.Raw)
				return nil
			}
			fmt.Fprint(c.OutOrStdout(), app.FormatState(st))
			return nil
		},
	}
	show.Flags().BoolVar(&raw, "raw", false, "print the full running-config as text")

	var desiredPath string
	plan := &cobra.Command{
		Use:   "plan",
		Short: "Show what apply would change (never touches the router)",
		RunE: func(c *cobra.Command, _ []string) error {
			a, err := app.New(*cfgPath)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close() }()
			want, err := app.LoadDesired(desiredPath)
			if err != nil {
				return err
			}
			res, err := a.Client.Apply(c.Context(), want, true)
			if err != nil {
				return err
			}
			fmt.Fprint(c.OutOrStdout(), res.Plan.String())
			return nil
		},
	}
	plan.Flags().StringVarP(&desiredPath, "desired", "d", "groups.yaml", "YAML file with desired groups")

	var dryRun, noSave bool
	apply := &cobra.Command{
		Use:   "apply",
		Short: "Apply the desired groups/routes to the router",
		RunE: func(c *cobra.Command, _ []string) error {
			a, err := app.New(*cfgPath)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close() }()
			if noSave {
				a.Client = a.WithoutSave()
			}
			want, err := app.LoadDesired(desiredPath)
			if err != nil {
				return err
			}
			started := time.Now()
			res, err := a.Client.Apply(c.Context(), want, dryRun)
			a.Record(journal.FromApply(started, time.Since(started), dryRun, res, err, a.Config.Journal.MaxChanges))
			if res != nil && res.Plan != nil {
				fmt.Fprint(c.OutOrStdout(), res.Plan.String())
			}
			if err != nil {
				return err
			}
			switch {
			case res.DryRun:
				fmt.Fprintln(c.OutOrStdout(), "dry-run: nothing was sent to the router")
			case res.Applied == 0:
				fmt.Fprintln(c.OutOrStdout(), "router already matches the desired state")
			default:
				fmt.Fprintf(c.OutOrStdout(), "applied %d command(s), saved=%v", res.Applied, res.Saved)
				if res.BackupPath != "" {
					fmt.Fprintf(c.OutOrStdout(), ", backup=%s", res.BackupPath)
				}
				fmt.Fprintln(c.OutOrStdout())
			}
			return nil
		},
	}
	apply.Flags().StringVarP(&desiredPath, "desired", "d", "groups.yaml", "YAML file with desired groups")
	apply.Flags().BoolVar(&dryRun, "dry-run", false, "print the plan without sending anything")
	apply.Flags().BoolVar(&noSave, "no-save", false, "do not run 'system configuration save' afterwards")

	save := &cobra.Command{
		Use:   "save",
		Short: "Run 'system configuration save' on the router",
		RunE: func(c *cobra.Command, _ []string) error {
			a, err := app.New(*cfgPath)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close() }()
			if err := a.Client.Save(c.Context()); err != nil {
				return err
			}
			fmt.Fprintln(c.OutOrStdout(), "configuration saved")
			return nil
		},
	}

	cmd.AddCommand(show, plan, apply, save)
	return cmd
}

func exitCode(err error) int {
	switch {
	case errors.Is(err, context.Canceled):
		return 130
	default:
		return 1
	}
}

func init() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
}
