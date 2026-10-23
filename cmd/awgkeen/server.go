package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/app"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/server"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/syncer"
)

// withServer opens the app and resolves --server (empty = the only one).
func withServer(cfgPath, name string, fn func(a *app.App, srv string, m *server.Manager) error) error {
	a, err := app.New(cfgPath)
	if err != nil {
		return err
	}
	defer func() { _ = a.Close() }()
	srv, err := a.DefaultServer(name)
	if err != nil {
		return err
	}
	m, err := a.Server(srv)
	if err != nil {
		return err
	}
	return fn(a, srv, m)
}

func newServerCmd(cfgPath *string) *cobra.Command {
	cmd := &cobra.Command{Use: "server", Short: "Run your own AmneziaWG server (VPS over SSH or this machine) or manage an Amnezia one"}
	var name string
	cmd.PersistentFlags().StringVarP(&name, "server", "s", "", "server name (config servers: or imported; default: the only one)")

	list := &cobra.Command{
		Use:   "list",
		Short: "List servers: from the config and imported from Amnezia admin keys",
		RunE: func(c *cobra.Command, _ []string) error {
			a, err := app.New(*cfgPath)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close() }()
			tw := tabwriter.NewWriter(c.OutOrStdout(), 0, 8, 2, ' ', 0)
			fmt.Fprintln(tw, "SERVER\tHOST\tENDPOINT\tSUBNET\tPEERS\tDEPLOYED")
			for _, n := range a.ServerNames() {
				sc, _ := a.ServerConfig(n)
				host, endpoint, subnet := sc.Host, sc.ClientEndpoint(), sc.Subnet
				switch {
				case sc.Local:
					host = "(local)"
				case sc.Amnezia:
					host += " (Amnezia)"
					endpoint, subnet = sc.Endpoint, "-"
				}
				peers, deployed := "not initialized", "-"
				if m, err := a.Server(n); err == nil {
					if st, err := m.Load(); err == nil {
						peers = fmt.Sprint(len(st.Peers))
						if !st.DeployedAt.IsZero() {
							deployed = st.DeployedAt.Local().Format("2006-01-02 15:04")
						}
						if h := st.Amnezia; h != nil {
							endpoint, subnet = fmt.Sprintf("%s:%d", sc.Endpoint, h.ListenPort), h.Subnet
						}
					}
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", n, host, endpoint, subnet, peers, deployed)
			}
			return tw.Flush()
		},
	}

	var force bool
	initCmd := &cobra.Command{
		Use:   "init",
		Short: "Generate the server keys, address and obfuscation parameters (Amnezia server: read them from its container)",
		RunE: func(c *cobra.Command, _ []string) error {
			return withServer(*cfgPath, name, func(a *app.App, srv string, m *server.Manager) error {
				st, err := a.InitServer(c.Context(), syncer.TriggerCLI, srv, force)
				if err != nil {
					return err
				}
				w := c.OutOrStdout()
				if st.Amnezia != nil {
					fmt.Fprintf(w, "Amnezia server %s read: container %s, interface %s, port %d, network %s\n", srv, st.Amnezia.Container, st.Amnezia.Interface, st.Amnezia.ListenPort, st.Amnezia.Subnet)
					fmt.Fprintf(w, "next: awgkeen peer add -s %s NAME\n", srv)
					return nil
				}
				fmt.Fprintf(w, "server %s initialized: address %s, public key %s\n", srv, st.Address, st.Keys.Public)
				fmt.Fprintf(w, "obfuscation: %s\n", strings.Join(st.Params.Lines(), ", "))
				fmt.Fprintf(w, "next: awgkeen server install (once), awgkeen peer add NAME\n")
				return nil
			})
		},
	}
	initCmd.Flags().BoolVar(&force, "force", false, "regenerate an initialized server (every client must be re-issued)")

	install := &cobra.Command{
		Use:   "install",
		Short: "Install amneziawg on the host (Ubuntu: Amnezia PPA; other systems: prints instructions)",
		RunE: func(c *cobra.Command, _ []string) error {
			return withServer(*cfgPath, name, func(_ *app.App, _ string, m *server.Manager) error {
				out, err := m.Install(c.Context())
				_, _ = io.WriteString(c.OutOrStdout(), out)
				return err
			})
		},
	}

	plan := &cobra.Command{
		Use:   "plan",
		Short: "Show what deploy would change on the host",
		RunE: func(c *cobra.Command, _ []string) error {
			return withServer(*cfgPath, name, func(_ *app.App, _ string, m *server.Manager) error {
				p, _, _, err := m.Plan(c.Context())
				if err != nil {
					return err
				}
				_, _ = io.WriteString(c.OutOrStdout(), p.String())
				return nil
			})
		},
	}

	var dryRun bool
	deploy := &cobra.Command{
		Use:   "deploy",
		Short: "Write the configuration to the host and bring the interface up (peers sync live)",
		RunE: func(c *cobra.Command, _ []string) error {
			return withServer(*cfgPath, name, func(a *app.App, srv string, _ *server.Manager) error {
				p, err := a.DeployServer(c.Context(), syncer.TriggerCLI, srv, dryRun)
				if p != nil {
					printDeploy(c.OutOrStdout(), p, dryRun)
				}
				return err
			})
		},
	}
	deploy.Flags().BoolVar(&dryRun, "dry-run", false, "print the plan without changing the host")

	status := &cobra.Command{
		Use:   "status",
		Short: "Show the interface and peers with handshakes and traffic; fails when the interface is down",
		RunE: func(c *cobra.Command, _ []string) error {
			return withServer(*cfgPath, name, func(_ *app.App, srv string, m *server.Manager) error {
				s, err := m.Status(c.Context())
				if err != nil {
					return err
				}
				w := c.OutOrStdout()
				if !s.Up {
					fmt.Fprintf(w, "server %s: interface %s is DOWN\n", srv, m.Config.Interface)
				} else {
					fmt.Fprintf(w, "server %s: interface %s up, port %d\n", srv, m.Config.Interface, s.ListenPort)
				}
				printPeerStatus(w, s.Peers)
				if !s.Up {
					return errors.New("server interface is down")
				}
				return nil
			})
		},
	}

	var replace bool
	importCmd := &cobra.Command{
		Use:   "import NAME vpn://ADMIN-KEY|FILE|-",
		Short: "Register a server installed by the AmneziaVPN app from its admin (full access) key",
		Args:  cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			data, err := readKeyArg(c, args[1])
			if err != nil {
				return err
			}
			if !server.IsAdminKey(data) {
				_, err := server.ParseAdminKey(string(data))
				return fmt.Errorf("%w; import client keys with `awgkeen awg import`", err)
			}
			a, err := app.New(*cfgPath)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close() }()
			res, err := a.ImportKey(c.Context(), syncer.TriggerCLI, args[0], data, false, replace)
			if res != nil {
				printKeyImport(c.OutOrStdout(), res)
			}
			return err
		},
	}
	importCmd.Flags().BoolVar(&replace, "replace", false, "replace the credentials of an imported server")

	remove := &cobra.Command{
		Use:   "remove NAME",
		Short: "Forget an imported server and its credentials (issued clients keep working)",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			a, err := app.New(*cfgPath)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close() }()
			if err := a.ForgetServer(args[0]); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "server %s forgotten; its clients are still on the server\n", args[0])
			return nil
		},
	}

	cmd.AddCommand(list, initCmd, install, importCmd, plan, deploy, status, remove)
	return cmd
}

func newPeerCmd(cfgPath *string) *cobra.Command {
	cmd := &cobra.Command{Use: "peer", Short: "Issue and revoke client keys of your AmneziaWG server"}
	var name string
	var noDeploy bool
	cmd.PersistentFlags().StringVarP(&name, "server", "s", "", "server name from servers: in the config (default: the only one)")

	op := func(use, short, opName string) *cobra.Command {
		return &cobra.Command{
			Use:   use + " NAME",
			Short: short,
			Args:  cobra.ExactArgs(1),
			RunE: func(c *cobra.Command, args []string) error {
				return withServer(*cfgPath, name, func(a *app.App, srv string, m *server.Manager) error {
					_, p, err := a.PeerOp(c.Context(), syncer.TriggerCLI, srv, opName, args[0], !noDeploy)
					if err != nil {
						return err
					}
					w := c.OutOrStdout()
					fmt.Fprintf(w, "peer %s: %s done\n", args[0], opName)
					if p != nil {
						printDeploy(w, p, false)
					} else {
						fmt.Fprintln(w, "not deployed yet: run `awgkeen server deploy`")
					}
					if opName == server.PeerRotate {
						fmt.Fprintf(w, "the old configuration no longer works; get the new one: awgkeen peer show %s\n", args[0])
					}
					return nil
				})
			},
		}
	}

	var show showFlags
	var asTunnel bool
	add := &cobra.Command{
		Use:   "add NAME",
		Short: "Create a client (address, keys) and deploy; prints its configuration",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return withServer(*cfgPath, name, func(a *app.App, srv string, m *server.Manager) error {
				_, p, err := a.PeerOp(c.Context(), syncer.TriggerCLI, srv, server.PeerAdd, args[0], !noDeploy)
				if err != nil {
					return err
				}
				w := c.ErrOrStderr()
				if p != nil {
					printDeploy(w, p, false)
				} else {
					fmt.Fprintln(w, "not deployed yet: run `awgkeen server deploy`")
				}
				if asTunnel {
					conf, err := m.ClientConf(args[0])
					if err != nil {
						return err
					}
					res, err := a.ImportKey(c.Context(), syncer.TriggerCLI, srv, []byte(conf), false, false)
					if err != nil {
						return err
					}
					printKeyImport(c.OutOrStdout(), res)
					return nil
				}
				return show.print(c.OutOrStdout(), m, args[0])
			})
		},
	}
	show.bind(add)
	add.Flags().BoolVar(&asTunnel, "as-tunnel", false, "also add the client to this awgkeen as tunnel <server> for the router (not applied)")

	list := &cobra.Command{
		Use:   "list",
		Short: "List clients with their addresses and last handshake",
		RunE: func(c *cobra.Command, _ []string) error {
			return withServer(*cfgPath, name, func(_ *app.App, _ string, m *server.Manager) error {
				st, err := m.Load()
				if err != nil {
					return err
				}
				var ps []server.PeerStatus
				if s, err := m.Status(c.Context()); err == nil {
					ps = s.Peers
				} else {
					fmt.Fprintf(c.ErrOrStderr(), "warning: no runtime status: %v\n", err)
					for _, p := range st.Peers {
						ps = append(ps, server.PeerStatus{Name: p.Name, PublicKey: p.Keys.Public, Address: p.Address, Disabled: p.Disabled})
					}
				}
				printPeerStatus(c.OutOrStdout(), ps)
				return nil
			})
		},
	}

	var show2 showFlags
	showCmd := &cobra.Command{
		Use:   "show NAME",
		Short: "Print a client's configuration (.conf, QR code or Amnezia vpn:// key)",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return withServer(*cfgPath, name, func(_ *app.App, _ string, m *server.Manager) error {
				return show2.print(c.OutOrStdout(), m, args[0])
			})
		},
	}
	show2.bind(showCmd)

	remove := op("remove", "Revoke a client: forget its keys and remove it from the server", server.PeerRemove)
	disable := op("disable", "Block a client without forgetting it", server.PeerDisable)
	enable := op("enable", "Unblock a disabled client", server.PeerEnable)
	rotate := op("rotate", "Issue new keys for a client (the old configuration stops working)", server.PeerRotate)
	for _, c := range []*cobra.Command{add, remove, disable, enable, rotate} {
		c.Flags().BoolVar(&noDeploy, "no-deploy", false, "only change the local state; deploy later")
	}
	cmd.AddCommand(add, list, showCmd, remove, disable, enable, rotate)
	return cmd
}

// showFlags selects the client configuration format.
type showFlags struct {
	qr, vpn bool
	png     string
}

func (f *showFlags) bind(c *cobra.Command) {
	c.Flags().BoolVar(&f.qr, "qr", false, "print a QR code for the AmneziaWG mobile app")
	c.Flags().BoolVar(&f.vpn, "vpn", false, "print an Amnezia vpn:// key for the AmneziaVPN app")
	c.Flags().StringVar(&f.png, "png", "", "write the QR code as a PNG file")
}

func (f *showFlags) print(w io.Writer, m *server.Manager, peer string) error {
	if f.vpn {
		key, err := m.VPNKey(peer)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(w, key)
		return err
	}
	conf, err := m.ClientConf(peer)
	if err != nil {
		return err
	}
	if f.png != "" {
		png, err := server.QRPNG(conf, 512)
		if err != nil {
			return err
		}
		if err := os.WriteFile(f.png, png, 0o600); err != nil {
			return err
		}
	}
	if f.qr {
		q, err := server.QRText(conf)
		if err != nil {
			return err
		}
		_, err = io.WriteString(w, q)
		return err
	}
	_, err = io.WriteString(w, conf)
	return err
}

func printDeploy(w io.Writer, p *server.DeployPlan, dryRun bool) {
	switch {
	case p.Empty():
		fmt.Fprintln(w, "server is up to date")
	case dryRun:
		fmt.Fprint(w, "would change:\n"+p.String())
	default:
		fmt.Fprint(w, "deployed:\n"+p.String())
		if p.Restart {
			fmt.Fprintln(w, "the interface was restarted")
		}
	}
}

func printPeerStatus(w io.Writer, ps []server.PeerStatus) {
	tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
	fmt.Fprintln(tw, "PEER\tADDRESS\tENDPOINT\tHANDSHAKE\tRX\tTX")
	for _, p := range ps {
		n := p.Name
		switch {
		case p.Unknown:
			n = p.PublicKey[:min(len(p.PublicKey), 8)] + "… (not managed)"
		case p.Disabled:
			n += " (disabled)"
		}
		hs := "never"
		if !p.LastHandshake.IsZero() {
			hs = time.Since(p.LastHandshake).Truncate(time.Second).String() + " ago"
		}
		ep := p.Endpoint
		if ep == "" {
			ep = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", n, p.Address, ep, hs, bytesHuman(p.RxBytes), bytesHuman(p.TxBytes))
	}
	_ = tw.Flush()
}

func bytesHuman(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
