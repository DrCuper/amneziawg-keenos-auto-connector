package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/api"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/app"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/awg"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/journal"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/syncer"
)

func newAWGCmd(cfgPath *string) *cobra.Command {
	cmd := &cobra.Command{Use: "awg", Short: "Import AmneziaWG configs onto the router and check the tunnels"}

	inspect := &cobra.Command{
		Use:   "inspect FILE",
		Short: "Parse a .conf or vpn:// key and print it without secrets (no router access)",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			data, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			conf, err := awg.Parse(data)
			if err != nil {
				return err
			}
			printConf(c.OutOrStdout(), conf)
			return nil
		},
	}

	list := &cobra.Command{
		Use:   "list",
		Short: "List configured tunnels and their router interfaces",
		RunE: func(c *cobra.Command, _ []string) error {
			a, err := app.New(*cfgPath)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close() }()
			m := a.Tunnels()
			recs, err := m.Records()
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(c.OutOrStdout(), 0, 8, 2, ' ', 0)
			fmt.Fprintln(tw, "TUNNEL\tINTERFACE\tCONFIG\tAPPLIED")
			seen := map[string]bool{}
			for _, t := range m.List() {
				seen[t.Name] = true
				r := recs[t.Name]
				iface := t.Interface
				if t.AutoInterface() {
					iface = "auto"
					if r.Interface != "" {
						iface = "auto → " + r.Interface
					}
				}
				src := t.ConfFile
				switch {
				case t.Imported:
					src = "(imported key)"
				case src == "":
					src = "(inline)"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", t.Name, iface, src, appliedAt(r))
			}
			var orphans []string
			for name := range recs {
				if !seen[name] {
					orphans = append(orphans, name)
				}
			}
			sort.Strings(orphans)
			for _, name := range orphans {
				fmt.Fprintf(tw, "%s\t%s\t(not in config: awgkeen awg remove %s)\t%s\n", name, recs[name].Interface, name, appliedAt(recs[name]))
			}
			return tw.Flush()
		},
	}

	plan := &cobra.Command{
		Use:   "plan [TUNNEL...]",
		Short: "Show what apply would change on the router (never touches it)",
		RunE: func(c *cobra.Command, args []string) error {
			return runAWGApply(c, *cfgPath, args, awg.ApplyOptions{DryRun: true}, false)
		},
	}

	var dryRun, noSave, forceKeys bool
	apply := &cobra.Command{
		Use:   "apply [TUNNEL...]",
		Short: "Create or update the tunnels' Wireguard interfaces on the router",
		RunE: func(c *cobra.Command, args []string) error {
			return runAWGApply(c, *cfgPath, args, awg.ApplyOptions{DryRun: dryRun, ForceKeys: forceKeys}, noSave)
		},
	}
	apply.Flags().BoolVar(&dryRun, "dry-run", false, "print the plan without sending anything")
	apply.Flags().BoolVar(&noSave, "no-save", false, "do not run 'system configuration save' afterwards")
	apply.Flags().BoolVar(&forceKeys, "force-keys", false, "re-send private/preshared keys even if unchanged")

	var rmDry bool
	remove := &cobra.Command{
		Use:   "remove TUNNEL",
		Short: "Delete the tunnel's interface from the router (only one created by awgkeen); an imported key is forgotten too",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			a, err := app.New(*cfgPath)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close() }()
			if t, ok := a.Tunnels().Tunnel(args[0]); ok && t.Imported && !rmDry {
				res, err := a.ForgetTunnel(c.Context(), syncer.TriggerCLI, args[0])
				if err == nil {
					fmt.Fprintf(c.OutOrStdout(), "imported key %s forgotten\n", args[0])
				}
				if res == nil {
					return err
				}
				return reportAWG(c.OutOrStdout(), res, err)
			}
			started := time.Now()
			res, err := a.Tunnels().Remove(c.Context(), args[0], rmDry)
			if res != nil {
				e := journal.FromApply(started, time.Since(started), rmDry, res.Apply, err, a.Config.Journal.MaxChanges)
				e.Kind = journal.KindAWGRemove
				a.Record(e)
			}
			return reportAWG(c.OutOrStdout(), res, err)
		},
	}
	remove.Flags().BoolVar(&rmDry, "dry-run", false, "print the plan without sending anything")

	status := &cobra.Command{
		Use:   "status [TUNNEL...]",
		Short: "Show interface state, peers and handshakes; fails when a tunnel is down",
		RunE: func(c *cobra.Command, args []string) error {
			a, err := app.New(*cfgPath)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close() }()
			sts, err := a.Tunnels().Statuses(c.Context(), args)
			if err != nil {
				return err
			}
			return printStatuses(c.OutOrStdout(), sts)
		},
	}

	var applyNow, replace bool
	importCmd := &cobra.Command{
		Use:   "import NAME FILE|vpn://KEY|-",
		Short: "Add an Amnezia key to awgkeen: a client key becomes a tunnel, an admin key a managed server (the router is not touched)",
		Long: `Adds what the key gives access to; the router is changed only with --apply
(or later with ` + "`awgkeen awg apply NAME`" + `).

A client key (.conf or vpn:// from the AmneziaVPN app, a friend's or
provider's server, or ` + "`awgkeen peer add`" + `) is stored in
state_dir/tunnels/NAME.conf (0600) as tunnel NAME. Import several keys under
different names to use several servers; route domain groups to them with
` + "`tunnel: NAME`" + ` in domains.yaml.

An admin key ("full access" share in the AmneziaVPN app) registers server
NAME: awgkeen connects over SSH, reads the AmneziaWG container and can then
issue and revoke clients with ` + "`awgkeen peer ... -s NAME`" + ` without the app.`,
		Args: cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			data, err := readKeyArg(c, args[1])
			if err != nil {
				return err
			}
			a, err := app.New(*cfgPath)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close() }()
			res, err := a.ImportKey(c.Context(), syncer.TriggerCLI, args[0], data, applyNow, replace)
			if res != nil {
				printKeyImport(c.OutOrStdout(), res)
			}
			return err
		},
	}
	importCmd.Flags().BoolVar(&applyNow, "apply", false, "also import the tunnel onto the router now (client keys)")
	importCmd.Flags().BoolVar(&replace, "replace", false, "replace the key of an imported tunnel or server (e.g. after it was re-issued)")

	cmd.AddCommand(inspect, list, plan, apply, importCmd, remove, status)
	return cmd
}

func runAWGApply(c *cobra.Command, cfgPath string, names []string, o awg.ApplyOptions, noSave bool) error {
	a, err := app.New(cfgPath)
	if err != nil {
		return err
	}
	defer func() { _ = a.Close() }()
	if noSave {
		a.Client = a.WithoutSave()
	}
	started := time.Now()
	res, err := a.Tunnels().Apply(c.Context(), names, o)
	if res != nil {
		e := journal.FromApply(started, time.Since(started), o.DryRun, res.Apply, err, a.Config.Journal.MaxChanges)
		e.Kind = journal.KindAWGApply
		a.Record(e)
	}
	return reportAWG(c.OutOrStdout(), res, err)
}

// applyTunnelsOnStart imports the tunnels before the daemon's first sync, so
// that `tunnel:` routes resolve. Failures are logged and alerted, not fatal.
func applyTunnelsOnStart(ctx context.Context, a *app.App) {
	if len(a.Tunnels().List()) == 0 || !a.Config.AWG.ApplyAtStart() {
		return
	}
	started := time.Now()
	res, err := a.Tunnels().Apply(ctx, nil, awg.ApplyOptions{})
	if res != nil {
		e := journal.FromApply(started, time.Since(started), false, res.Apply, err, a.Config.Journal.MaxChanges)
		e.Kind, e.Trigger = journal.KindAWGApply, syncer.TriggerStartup
		a.Record(e)
		for _, it := range res.Items {
			for _, w := range it.Warnings {
				a.Logger.Warn("awg: "+w, "tunnel", it.Tunnel)
			}
		}
	}
	if err != nil {
		a.Logger.Error("awg: import at startup failed", "err", err)
		return
	}
	if res.Apply != nil && res.Apply.Applied > 0 {
		a.Logger.Info("awg: tunnels imported", "commands", res.Apply.Applied)
	}
}

// readKeyArg reads a key given literally (vpn://...), from stdin (-) or a file.
func readKeyArg(c *cobra.Command, src string) ([]byte, error) {
	switch {
	case strings.HasPrefix(src, "vpn://"):
		return []byte(src), nil
	case src == "-":
		return io.ReadAll(io.LimitReader(c.InOrStdin(), 1<<20))
	default:
		return os.ReadFile(src) //nolint:gosec // operator-supplied path
	}
}

func printKeyImport(w io.Writer, r *api.KeyImport) {
	switch r.Kind {
	case api.KeyServer:
		fmt.Fprintf(w, "admin key: server %s registered", r.Name)
		if !r.Adopted {
			fmt.Fprintln(w, " (not read yet)")
			return
		}
		fmt.Fprintf(w, " and read (clients connect to %s)\n", r.Endpoint)
		fmt.Fprintf(w, "issue a client: awgkeen peer add -s %s NAME   (for this router: --as-tunnel)\n", r.Name)
	default:
		if r.Applied {
			fmt.Fprintf(w, "tunnel %s imported onto the router:", r.Name)
			for _, it := range r.Items {
				fmt.Fprintf(w, " %s", it.Interface)
			}
			fmt.Fprintln(w)
			if r.Plan != "" {
				fmt.Fprint(w, r.Plan)
			}
			return
		}
		fmt.Fprintf(w, "client key saved as tunnel %s; the router is unchanged\nput it on the router: awgkeen awg apply %s\n", r.Name, r.Name)
	}
}

func reportAWG(w io.Writer, res *awg.Result, err error) error {
	if res != nil {
		for _, it := range res.Items {
			state := "update"
			if it.Created {
				state = "create"
			}
			fmt.Fprintf(w, "tunnel %s -> %s (%s)\n", it.Tunnel, it.Interface, state)
			for _, warn := range it.Warnings {
				fmt.Fprintf(w, "  warning: %s\n", warn)
			}
		}
		if res.Apply != nil && res.Apply.Plan != nil {
			fmt.Fprint(w, "\n"+res.Apply.Plan.String())
		}
	}
	if err != nil {
		return err
	}
	switch {
	case res.DryRun:
		fmt.Fprintln(w, "dry-run: nothing was sent to the router")
	case res.Apply == nil || res.Apply.Applied == 0:
		fmt.Fprintln(w, "router already matches the tunnel configuration")
	default:
		fmt.Fprintf(w, "applied %d command(s), saved=%v", res.Apply.Applied, res.Apply.Saved)
		if res.Apply.BackupPath != "" {
			fmt.Fprintf(w, ", backup=%s", res.Apply.BackupPath)
		}
		fmt.Fprintln(w)
	}
	return nil
}

func printConf(w io.Writer, c *awg.Conf) {
	if pub, err := c.PublicKey(); err == nil {
		fmt.Fprintf(w, "public key: %s\n", pub)
	}
	fmt.Fprintf(w, "address:    %s\n", strings.Join(c.Addresses, ", "))
	if c.MTU != 0 {
		fmt.Fprintf(w, "mtu:        %d\n", c.MTU)
	}
	if v := c.ASCValues(); v != nil {
		fmt.Fprintf(w, "asc:        %s\n", strings.Join(quoteEmpty(v), " "))
	} else {
		fmt.Fprintln(w, "asc:        none (plain WireGuard)")
	}
	for i, p := range c.Peers {
		fmt.Fprintf(w, "peer #%d:    %s endpoint=%s allowed=%s keepalive=%d psk=%v\n",
			i+1, p.PublicKey, p.Endpoint, strings.Join(p.AllowedIPs, ","), p.Keepalive, p.PresharedKey != "")
	}
	for _, warn := range c.Warnings {
		fmt.Fprintf(w, "warning: %s\n", warn)
	}
}

func quoteEmpty(v []string) []string {
	out := make([]string, len(v))
	for i, s := range v {
		out[i] = s
		if s == "" || strings.ContainsAny(s, " \t") {
			out[i] = `"` + s + `"`
		}
	}
	return out
}

func printStatuses(w io.Writer, sts []awg.Status) error {
	failed := 0
	for _, s := range sts {
		state := "ok"
		if s.Err != nil {
			state = "DOWN: " + s.Err.Error()
			failed++
		}
		iface := s.Interface
		if iface == "" {
			iface = "-"
		}
		fmt.Fprintf(w, "tunnel %s (%s): %s\n", s.Tunnel, iface, state)
		if s.Router == nil || !s.Router.Exists {
			continue
		}
		fmt.Fprintf(w, "  up=%v connected=%v\n", s.Router.Up, s.Router.Connected)
		for _, p := range s.Router.Peers {
			hs := "never"
			if p.LastHandshake >= 0 {
				hs = p.LastHandshake.String() + " ago"
			}
			fmt.Fprintf(w, "  peer %s endpoint=%s handshake=%s rx=%d tx=%d\n", short(p.PublicKey), p.Endpoint, hs, p.RxBytes, p.TxBytes)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d tunnel(s) down", failed)
	}
	return nil
}

func short(k string) string {
	if len(k) > 8 {
		return k[:8] + "…"
	}
	return k
}

func appliedAt(r awg.Record) string {
	if r.AppliedAt.IsZero() {
		if r.Interface != "" {
			return "reserved, not applied"
		}
		return "never"
	}
	return r.AppliedAt.Local().Format("2006-01-02 15:04:05")
}
