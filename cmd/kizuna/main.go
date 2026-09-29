package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/osuki-dev/kizuna/internal/adapter/api"
	"github.com/osuki-dev/kizuna/internal/adapter/client"
	"github.com/osuki-dev/kizuna/internal/adapter/config"
	"github.com/osuki-dev/kizuna/internal/adapter/presenter"
	"github.com/osuki-dev/kizuna/internal/adapter/strategy"
	"github.com/osuki-dev/kizuna/internal/domain/entity"
	"github.com/osuki-dev/kizuna/internal/infrastructure/auth"
	"github.com/osuki-dev/kizuna/internal/infrastructure/backup"
	"github.com/osuki-dev/kizuna/internal/infrastructure/gossip"
	"github.com/osuki-dev/kizuna/internal/infrastructure/i18n"
	"github.com/osuki-dev/kizuna/internal/infrastructure/ingress"
	"github.com/osuki-dev/kizuna/internal/infrastructure/mesh"
	"github.com/osuki-dev/kizuna/internal/infrastructure/service"
	"github.com/osuki-dev/kizuna/internal/infrastructure/telemetry"
	"github.com/osuki-dev/kizuna/internal/infrastructure/updater"
	"github.com/osuki-dev/kizuna/internal/infrastructure/workload"
	"github.com/osuki-dev/kizuna/internal/usecase"
	"github.com/spf13/cobra"
)

var (
	version   = "dev"
	commit    = "none"
	buildTime = "unknown"

	// Global CLI flags
	flagEnv     string
	flagConfig  string
	flagJSON    bool
	flagNoColor bool
	flagLang    string
)

func main() {
	rootCmd := &cobra.Command{
		Use:     "kizuna",
		Short:   i18n.T("app_desc"),
		Version: fmt.Sprintf("%s (commit: %s, built: %s)", version, commit, buildTime),
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			if flagLang != "" {
				i18n.SetLanguage(flagLang)
			}
			presenter.JSONOutput = flagJSON
			presenter.NoColor = flagNoColor
			if flagNoColor || os.Getenv("NO_COLOR") != "" {
				lipgloss.SetColorProfile(0)
			}
		},
		PersistentPostRun: func(cmd *cobra.Command, args []string) {
			if !flagJSON && cmd.Name() != "upgrade" && cmd.Name() != "update" {
				mgr := updater.NewManager("", "")
				if notice := mgr.CheckUpdateNotice(version); notice != "" {
					fmt.Print(notice)
				}
			}
		},
	}

	rootCmd.PersistentFlags().StringVarP(&flagEnv, "env", "e", "", "Target environment (development, staging, production, test)")
	rootCmd.PersistentFlags().StringVarP(&flagConfig, "config", "c", "", "Path to kizuna.yaml")
	rootCmd.PersistentFlags().StringVarP(&flagLang, "lang", "l", "", "Language code: en, ja, zh (default: system language)")
	rootCmd.PersistentFlags().BoolVar(&flagJSON, "json", false, "Output results in JSON format for AI agents & automation")
	rootCmd.PersistentFlags().BoolVar(&flagNoColor, "no-color", false, "Disable colored ANSI styling")

	// Custom Lipgloss Help Template
	setupHelp(rootCmd)

	// Workloads & Apps
	rootCmd.AddCommand(newDeployCmd())
	rootCmd.AddCommand(newScaleCmd())
	rootCmd.AddCommand(newRollbackCmd())
	rootCmd.AddCommand(newStatusCmd())
	rootCmd.AddCommand(newDashboardCmd())
	rootCmd.AddCommand(newLogsCmd())
	rootCmd.AddCommand(newTunnelCmd())

	// Mesh & Services Infrastructure
	rootCmd.AddCommand(newServiceCmd())
	rootCmd.AddCommand(newNodeCmd())

	// Configuration & Utilities
	rootCmd.AddCommand(newInitCmd())
	rootCmd.AddCommand(newCheckCmd())
	rootCmd.AddCommand(newBackupCmd())
	rootCmd.AddCommand(newUpgradeCmd())

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func setupHelp(cmd *cobra.Command) {
	cmd.SetHelpFunc(func(c *cobra.Command, args []string) {
		// Early language resolution for help display
		if flagLang != "" {
			i18n.SetLanguage(flagLang)
		} else {
			for i, arg := range os.Args {
				if (arg == "--lang" || arg == "-l") && i+1 < len(os.Args) {
					i18n.SetLanguage(os.Args[i+1])
					break
				}
				if strings.HasPrefix(arg, "--lang=") {
					i18n.SetLanguage(strings.TrimPrefix(arg, "--lang="))
					break
				}
			}
		}

		ui := presenter.NewUI("")
		if flagJSON {
			subList := []map[string]string{}
			for _, sub := range c.Commands() {
				if !sub.Hidden {
					subList = append(subList, map[string]string{"name": sub.Name(), "summary": sub.Short})
				}
			}
			if c != cmd {
				_ = presenter.PrintJSON(os.Stdout, map[string]any{
					"command":     c.Name(),
					"usage":       c.UseLine(),
					"description": c.Short,
					"subcommands": subList,
				})
				return
			}
			_ = presenter.PrintJSON(os.Stdout, map[string]any{
				"cli":         "kizuna",
				"version":     version,
				"commands":    subList,
				"environment": flagEnv,
			})
			return
		}

		if c != cmd {
			fmt.Printf("%s %s\n", ui.Badge(" 絆 KIZUNA ", ui.Theme.Primary, "#FFF"), ui.BoldStyle.Render(c.CommandPath()))

			desc := c.Short
			descKey := "cmd_" + c.Name() + "_desc"
			if c.Parent() != nil && c.Parent() != cmd {
				parentKey := "cmd_" + c.Parent().Name() + "_" + c.Name() + "_desc"
				if i18n.Has(parentKey) {
					desc = i18n.T(parentKey)
				} else if i18n.Has(descKey) {
					desc = i18n.T(descKey)
				}
			} else if i18n.Has(descKey) {
				desc = i18n.T(descKey)
			}

			if desc != "" {
				fmt.Printf("\n%s\n", ui.MutedStyle.Render(desc))
			}
			fmt.Printf("\n%s %s\n", ui.BoldStyle.Render(i18n.T("help_usage")), ui.SecondaryStyle.Render(c.UseLine()))

			subcmds := c.Commands()
			if len(subcmds) > 0 {
				fmt.Printf("\n%s\n", ui.PrimaryStyle.Bold(true).Render(i18n.T("help_subcommands")))
				for _, sub := range subcmds {
					if !sub.Hidden {
						subDesc := sub.Short
						subKey := "cmd_" + c.Name() + "_" + sub.Name() + "_desc"
						if i18n.Has(subKey) {
							subDesc = i18n.T(subKey)
						} else if i18n.Has("cmd_" + sub.Name() + "_desc") {
							subDesc = i18n.T("cmd_" + sub.Name() + "_desc")
						}
						fmt.Printf("  %-16s %s\n", ui.SecondaryStyle.Bold(true).Render(sub.Name()), ui.MutedStyle.Render(subDesc))
					}
				}
			}

			localFlags := c.LocalFlags().FlagUsages()
			if localFlags != "" {
				fmt.Printf("\n%s\n%s", ui.BoldStyle.Render(i18n.T("help_flags")), localFlags)
			}
			return
		}

		fmt.Println(ui.Banner())

		pStyle := ui.PrimaryStyle.Bold(true)
		bStyle := ui.BoldStyle
		mStyle := ui.MutedStyle

		fmt.Printf("%s %s\n\n", bStyle.Render(i18n.T("help_usage")), mStyle.Render("kizuna <command> [flags]"))

		fmt.Printf("%s\n", pStyle.Render(i18n.T("help_workloads")))
		printCmdRow("deploy", i18n.T("cmd_deploy_desc"), ui)
		printCmdRow("scale", i18n.T("cmd_scale_desc"), ui)
		printCmdRow("rollback", i18n.T("cmd_rollback_desc"), ui)
		printCmdRow("status", i18n.T("cmd_status_desc"), ui)
		printCmdRow("dashboard", i18n.T("cmd_dashboard_desc"), ui)
		printCmdRow("logs", i18n.T("cmd_logs_desc"), ui)
		printCmdRow("tunnel", i18n.T("cmd_tunnel_desc"), ui)

		fmt.Printf("\n%s\n", pStyle.Render(i18n.T("help_infra")))
		printCmdRow("service", i18n.T("cmd_service_desc"), ui)
		printCmdRow("node", i18n.T("cmd_node_desc"), ui)

		fmt.Printf("\n%s\n", pStyle.Render(i18n.T("help_config")))
		printCmdRow("init", i18n.T("cmd_init_desc"), ui)
		printCmdRow("check", i18n.T("cmd_check_desc"), ui)
		printCmdRow("backup", i18n.T("cmd_backup_desc"), ui)
		printCmdRow("upgrade", i18n.T("cmd_upgrade_desc"), ui)

		fmt.Printf("\n%s\n", bStyle.Render(i18n.T("help_global_flags")))
		fmt.Printf("  %-18s %s\n", "-e, --env <name>", mStyle.Render(i18n.T("flag_env_desc")))
		fmt.Printf("  %-18s %s\n", "-c, --config <file>", mStyle.Render(i18n.T("flag_config_desc")))
		fmt.Printf("  %-18s %s\n", "-l, --lang <code>", mStyle.Render(i18n.T("flag_lang_desc")))
		fmt.Printf("  %-18s %s\n", "--json", mStyle.Render(i18n.T("flag_json_desc")))
		fmt.Printf("  %-18s %s\n", "--no-color", mStyle.Render(i18n.T("flag_nocolor_desc")))
		fmt.Printf("  %-18s %s\n", "-h, --help", mStyle.Render(i18n.T("flag_help_desc")))
		fmt.Printf("  %-18s %s\n\n", "-v, --version", mStyle.Render(i18n.T("flag_version_desc")))

		fmt.Printf("%s %s\n", mStyle.Render(i18n.T("help_learn_more")), "https://github.com/osuki-dev/kizuna")
	})
}

func printCmdRow(name, desc string, ui *presenter.UI) {
	fmt.Printf("  %-12s  %s\n", ui.SecondaryStyle.Bold(true).Render(name), ui.MutedStyle.Render(desc))
}

func elevateWithSudo() error {
	if runtime.GOOS == "windows" {
		return fmt.Errorf("managing system services on Windows requires running the command prompt or PowerShell as Administrator")
	}
	if os.Geteuid() == 0 {
		return nil
	}
	sudoPath, err := exec.LookPath("sudo")
	if err != nil {
		return fmt.Errorf("sudo is required for this action but was not found in PATH")
	}

	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to determine executable path: %w", err)
	}

	args := append([]string{execPath}, os.Args[1:]...)
	cmd := exec.Command(sudoPath, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to run with sudo: %w", err)
	}

	os.Exit(0)
	return nil
}

func newServiceCmd() *cobra.Command {
	svcCmd := &cobra.Command{
		Use:   "service",
		Short: "Manage the Kizuna background service daemon on this node",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServer()
		},
	}

	runCmd := &cobra.Command{
		Use:   "run",
		Short: "Run the Kizuna background service in the foreground",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServer()
		},
	}

	var flagUser bool
	var flagSystem bool

	installCmd := &cobra.Command{
		Use:   "install",
		Short: i18n.T("cmd_service_install_desc"),
		RunE: func(cmd *cobra.Command, args []string) error {
			isRoot := os.Geteuid() == 0
			useUserService := true

			if flagSystem {
				useUserService = false
			} else if flagUser {
				useUserService = true
			} else {
				// Default: root -> system service; non-root -> user service (no sudo/root required)
				useUserService = !isRoot
			}

			// If system daemon was explicitly requested by a non-root user, elevate using sudo
			if !useUserService && !isRoot {
				if err := elevateWithSudo(); err != nil {
					return fmt.Errorf("installing as a system daemon requires root permissions. Please run with sudo: 'sudo kizuna service install --system' or install as a user service: 'kizuna service install --user': %w", err)
				}
				return nil
			}

			daemonCfg := service.DaemonConfig{
				Name:        "kizuna-server",
				DisplayName: "Kizuna Service",
				Description: "Kizuna P2P Zero-Trust Deployment Service",
				Arguments:   []string{"service", "run"},
				UserService: useUserService,
			}
			mgr, err := service.NewManager(daemonCfg, func() {
				_ = runServer()
			})
			if err != nil {
				return err
			}
			if err := mgr.Install(); err != nil {
				return err
			}
			_ = mgr.Start()

			if useUserService {
				fmt.Println("✓ Kizuna user background service installed, registered, and started.")
				fmt.Println("  (Running as user service. To install as a system-wide service instead, run with '--system')")
			} else {
				fmt.Println("✓ Kizuna system background service installed, registered, and started.")
			}
			return nil
		},
	}
	installCmd.Flags().BoolVar(&flagUser, "user", false, "Install as user-level background service (default for non-root, no password required)")
	installCmd.Flags().BoolVar(&flagSystem, "system", false, "Install as system-wide daemon (requires administrator/root)")

	startCmd := &cobra.Command{
		Use:   "start",
		Short: i18n.T("cmd_service_start_desc"),
		RunE: func(cmd *cobra.Command, args []string) error {
			useUser := service.DetectUserService("kizuna-server")
			if !useUser && os.Geteuid() != 0 {
				if err := elevateWithSudo(); err != nil {
					return fmt.Errorf("service is installed as system daemon, please run with sudo: 'sudo kizuna service start': %w", err)
				}
				return nil
			}
			daemonCfg := service.DaemonConfig{Name: "kizuna-server", UserService: useUser}
			mgr, err := service.NewManager(daemonCfg, nil)
			if err != nil {
				return err
			}
			if err := mgr.Start(); err != nil {
				return err
			}
			fmt.Println("✓ Kizuna background service started.")
			return nil
		},
	}

	stopCmd := &cobra.Command{
		Use:   "stop",
		Short: i18n.T("cmd_service_stop_desc"),
		RunE: func(cmd *cobra.Command, args []string) error {
			useUser := service.DetectUserService("kizuna-server")
			if !useUser && os.Geteuid() != 0 {
				if err := elevateWithSudo(); err != nil {
					return fmt.Errorf("service is installed as system daemon, please run with sudo: 'sudo kizuna service stop': %w", err)
				}
				return nil
			}
			daemonCfg := service.DaemonConfig{Name: "kizuna-server", UserService: useUser}
			mgr, err := service.NewManager(daemonCfg, nil)
			if err != nil {
				return err
			}
			if err := mgr.Stop(); err != nil {
				return err
			}
			fmt.Println("✓ Kizuna background service stopped.")
			return nil
		},
	}

	restartCmd := &cobra.Command{
		Use:   "restart",
		Short: "Restart the Kizuna background service",
		RunE: func(cmd *cobra.Command, args []string) error {
			useUser := service.DetectUserService("kizuna-server")
			if !useUser && os.Geteuid() != 0 {
				if err := elevateWithSudo(); err != nil {
					return fmt.Errorf("service is installed as system daemon, please run with sudo: 'sudo kizuna service restart': %w", err)
				}
				return nil
			}
			daemonCfg := service.DaemonConfig{Name: "kizuna-server", UserService: useUser}
			mgr, err := service.NewManager(daemonCfg, nil)
			if err != nil {
				return err
			}
			if err := mgr.Restart(); err != nil {
				return err
			}
			fmt.Println("✓ Kizuna background service restarted.")
			return nil
		},
	}

	serviceStatusCmd := &cobra.Command{
		Use:   "status",
		Short: i18n.T("cmd_service_status_desc"),
		RunE: func(cmd *cobra.Command, args []string) error {
			useUser := service.DetectUserService("kizuna-server")
			daemonCfg := service.DaemonConfig{Name: "kizuna-server", UserService: useUser}
			mgr, err := service.NewManager(daemonCfg, nil)
			if err != nil {
				return err
			}
			st, err := mgr.GetStatus()
			if err != nil {
				return err
			}
			if flagJSON {
				return presenter.PrintJSON(os.Stdout, map[string]any{"service": "kizuna-server", "status": string(st)})
			}
			fmt.Printf("Kizuna background service status: %s\n", string(st))
			return nil
		},
	}

	uninstallCmd := &cobra.Command{
		Use:   "uninstall",
		Short: i18n.T("cmd_service_uninstall_desc"),
		RunE: func(cmd *cobra.Command, args []string) error {
			useUser := service.DetectUserService("kizuna-server")
			if !useUser && os.Geteuid() != 0 {
				if err := elevateWithSudo(); err != nil {
					return fmt.Errorf("service is installed as system daemon, please run with sudo: 'sudo kizuna service uninstall': %w", err)
				}
				return nil
			}
			daemonCfg := service.DaemonConfig{Name: "kizuna-server", UserService: useUser}
			mgr, err := service.NewManager(daemonCfg, nil)
			if err != nil {
				return err
			}
			_ = mgr.Stop()
			if err := mgr.Uninstall(); err != nil {
				return err
			}
			fmt.Println("✓ Kizuna background service uninstalled.")
			return nil
		},
	}

	svcCmd.AddCommand(runCmd, installCmd, startCmd, stopCmd, restartCmd, serviceStatusCmd, uninstallCmd)
	return svcCmd
}

func runServer() error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	meshGw := mesh.NewMeshGateway()
	defer func() { _ = meshGw.Close() }()

	workloadRunner := workload.NewWorkloadRunner("")
	ingressMgr := ingress.NewCaddyManager("")
	backupMgr := backup.NewBackupManager("")
	authStore, err := auth.NewAuthStore("")
	if err != nil {
		return err
	}

	nodeName, _ := os.Hostname()
	if nodeName == "" {
		nodeName = "kizuna-node"
	}
	nodeID := "node_" + nodeName

	apiServer := api.NewServer(authStore, workloadRunner, ingressMgr, backupMgr, nodeID, nodeName)

	addr, err := meshGw.Listen(ctx, 19800, func(conn net.Conn) {
		apiServer.ServeConn(conn)
	})
	if err != nil {
		return fmt.Errorf("failed to start mesh listener: %w", err)
	}

	// Initialize and launch decentralized Gossip Engine
	nodeRepo, _ := config.NewNodeRepository("")
	var seeds []*entity.Node
	if nodeRepo != nil {
		seeds, _ = nodeRepo.ListNodes()
	}

	gossipTransport := gossip.NewMeshTransport(meshGw)
	gossipEng := gossip.NewEngine(gossip.Config{
		NodeID:    nodeID,
		NodeName:  nodeName,
		MeshAddr:  addr,
		Transport: gossipTransport,
		Repo:      nodeRepo,
		Seeds:     seeds,
	})
	apiServer.SetGossipEngine(gossipEng)
	_ = gossipEng.Start(ctx)
	defer func() { _ = gossipEng.Stop() }()

	pin, _ := authStore.GetActivePIN()
	ui := presenter.NewUI("")

	if flagJSON {
		_ = presenter.PrintJSON(os.Stdout, map[string]any{
			"status":       "running",
			"node_id":      nodeID,
			"node_name":    nodeName,
			"mesh_address": addr,
			"pairing_pin":  pin,
			"mesh_peers":   len(gossipEng.GetMembers()),
		})
	} else {
		fmt.Println(ui.RenderServerStart(addr, pin))
	}

	<-ctx.Done()
	if !flagJSON {
		fmt.Println("\nShutting down Kizuna Server...")
	}
	return nil
}

func newNodeCmd() *cobra.Command {
	nodeCmd := &cobra.Command{
		Use:   "node",
		Short: "List, pair, or manage remote mesh nodes",
		RunE: func(cmd *cobra.Command, args []string) error {
			return listNodes()
		},
	}

	var pin string
	var name string

	addCmd := &cobra.Command{
		Use:   "add <mesh-address>",
		Short: "Pair with a remote Kizuna Server using its address and PIN",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			addr := args[0]
			repo, err := config.NewNodeRepository("")
			if err != nil {
				return err
			}
			meshGw := mesh.NewMeshGateway()
			defer func() { _ = meshGw.Close() }()

			cli := client.NewMeshClient(meshGw)
			res, err := cli.Pair(context.Background(), addr, 19800, pin, "kizuna-cli")
			if err != nil {
				return fmt.Errorf("%s", i18n.T("node_pair_failed", err))
			}
			if !res.Success {
				return fmt.Errorf("agent error: %s", res.Error)
			}

			if name == "" {
				name = res.NodeName
			}
			node := &entity.Node{
				ID:        res.NodeID,
				Name:      name,
				Addr:      addr,
				AuthToken: res.AuthToken,
				IsOnline:  true,
			}
			if err := repo.SaveNode(node); err != nil {
				return err
			}

			if flagJSON {
				return presenter.PrintJSON(os.Stdout, map[string]any{
					"success": true,
					"node":    node,
				})
			}

			ui := presenter.NewUI("")
			fmt.Printf("✓ %s\n", ui.SecondaryStyle.Bold(true).Render(i18n.T("node_pair_success", name)))
			return nil
		},
	}
	addCmd.Flags().StringVarP(&pin, "pin", "p", "", "6-digit pairing PIN shown on the server")
	addCmd.Flags().StringVarP(&name, "name", "n", "", "Alias for the remote node")
	_ = addCmd.MarkFlagRequired("pin")

	listCmd := &cobra.Command{
		Use:   "ls",
		Short: "List all paired mesh nodes",
		RunE: func(cmd *cobra.Command, args []string) error {
			return listNodes()
		},
	}

	rmCmd := &cobra.Command{
		Use:   "rm <node-name>",
		Short: "Remove a paired mesh node from the repository",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := config.NewNodeRepository("")
			if err != nil {
				return err
			}
			if err := repo.DeleteNode(args[0]); err != nil {
				return err
			}
			if flagJSON {
				return presenter.PrintJSON(os.Stdout, map[string]any{"success": true, "removed": args[0]})
			}
			fmt.Printf("✓ Removed node '%s'\n", args[0])
			return nil
		},
	}

	tagCmd := &cobra.Command{
		Use:   "tag <node-name> [add|rm|set] [tags...]",
		Short: "Manage labels and tags for a node with automatic remote sync",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			nodeName := args[0]
			repo, err := config.NewNodeRepository("")
			if err != nil {
				return err
			}
			node, err := repo.GetNode(nodeName)
			if err != nil {
				return fmt.Errorf("node '%s' not found: %w", nodeName, err)
			}

			// If no tags provided, display current tags
			if len(args) == 1 {
				if flagJSON {
					return presenter.PrintJSON(os.Stdout, map[string]any{
						"node": nodeName,
						"tags": node.Tags,
						"host": node.Host,
						"ip":   node.IP,
					})
				}
				if len(node.Tags) == 0 {
					fmt.Printf("Node '%s' has no tags assigned.\n", nodeName)
				} else {
					fmt.Printf("Node '%s' tags: [%s]\n", nodeName, strings.Join(node.Tags, ", "))
				}
				return nil
			}

			action := args[1]
			var tagList []string

			switch action {
			case "add":
				tagList = parseTags(args[2:])
				tagMap := make(map[string]bool)
				for _, t := range node.Tags {
					tagMap[t] = true
				}
				for _, t := range tagList {
					tagMap[t] = true
				}
				node.Tags = make([]string, 0, len(tagMap))
				for t := range tagMap {
					node.Tags = append(node.Tags, t)
				}
				sort.Strings(node.Tags)

			case "rm", "remove", "del":
				toRemove := parseTags(args[2:])
				removeMap := make(map[string]bool)
				for _, t := range toRemove {
					removeMap[t] = true
				}
				var updated []string
				for _, t := range node.Tags {
					if !removeMap[t] {
						updated = append(updated, t)
					}
				}
				node.Tags = updated

			case "set":
				node.Tags = parseTags(args[2:])
				sort.Strings(node.Tags)

			default:
				// If action is not add/rm/set, treat all remaining args as tags to add
				allTags := parseTags(args[1:])
				tagMap := make(map[string]bool)
				for _, t := range node.Tags {
					tagMap[t] = true
				}
				for _, t := range allTags {
					tagMap[t] = true
				}
				node.Tags = make([]string, 0, len(tagMap))
				for t := range tagMap {
					node.Tags = append(node.Tags, t)
				}
				sort.Strings(node.Tags)
			}

			// 1. Save locally
			if err := repo.SaveNode(node); err != nil {
				return fmt.Errorf("failed to save node locally: %w", err)
			}

			// 2. Sync to remote node via Mesh
			meshGw := mesh.NewMeshGateway()
			defer func() { _ = meshGw.Close() }()
			cli := client.NewMeshClient(meshGw)

			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			defer cancel()

			syncErr := cli.UpdateNodeMeta(ctx, node, entity.NodeMetaUpdate{
				Tags: node.Tags,
				Host: node.Host,
				IP:   node.IP,
			})

			if flagJSON {
				return presenter.PrintJSON(os.Stdout, map[string]any{
					"success":     true,
					"node":        node.Name,
					"tags":        node.Tags,
					"remote_sync": syncErr == nil,
				})
			}

			fmt.Printf("✓ Node '%s' tags updated: [%s]\n", node.Name, strings.Join(node.Tags, ", "))
			if syncErr != nil {
				fmt.Printf("  ⚠ Remote node '%s' is currently offline or unreachable (%v).\n    Updated locally; will sync automatically on next connection.\n", node.Name, syncErr)
			} else {
				fmt.Printf("  ✓ Successfully synced metadata to remote node '%s' via Mesh RPC.\n", node.Name)
			}
			return nil
		},
	}

	var flagHost string
	var flagIP string
	var flagTags string

	setCmd := &cobra.Command{
		Use:   "set <node-name>",
		Short: "Set node properties (host, ip, tags) with automatic remote sync",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			nodeName := args[0]
			repo, err := config.NewNodeRepository("")
			if err != nil {
				return err
			}
			node, err := repo.GetNode(nodeName)
			if err != nil {
				return fmt.Errorf("node '%s' not found: %w", nodeName, err)
			}

			if flagHost != "" {
				node.Host = flagHost
				if node.IP == "" {
					node.IP = flagHost
				}
			}
			if flagIP != "" {
				node.IP = flagIP
				if node.Host == "" {
					node.Host = flagIP
				}
			}
			if cmd.Flags().Changed("tags") {
				node.Tags = parseTags(strings.Split(flagTags, ","))
				sort.Strings(node.Tags)
			}

			if err := repo.SaveNode(node); err != nil {
				return fmt.Errorf("failed to save node locally: %w", err)
			}

			// Sync to remote
			meshGw := mesh.NewMeshGateway()
			defer func() { _ = meshGw.Close() }()
			cli := client.NewMeshClient(meshGw)

			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			defer cancel()

			syncErr := cli.UpdateNodeMeta(ctx, node, entity.NodeMetaUpdate{
				Tags: node.Tags,
				Host: node.Host,
				IP:   node.IP,
			})

			if flagJSON {
				return presenter.PrintJSON(os.Stdout, map[string]any{
					"success":     true,
					"node":        node,
					"remote_sync": syncErr == nil,
				})
			}

			fmt.Printf("✓ Node '%s' properties updated:\n", node.Name)
			if node.Host != "" {
				fmt.Printf("  Host: %s\n", node.Host)
			}
			if node.IP != "" && node.IP != node.Host {
				fmt.Printf("  IP:   %s\n", node.IP)
			}
			if len(node.Tags) > 0 {
				fmt.Printf("  Tags: [%s]\n", strings.Join(node.Tags, ", "))
			}
			if syncErr != nil {
				fmt.Printf("  ⚠ Remote node '%s' is currently offline or unreachable (%v).\n    Updated locally; will sync automatically on next connection.\n", node.Name, syncErr)
			} else {
				fmt.Printf("  ✓ Successfully synced metadata to remote node '%s' via Mesh RPC.\n", node.Name)
			}
			return nil
		},
	}
	setCmd.Flags().StringVar(&flagHost, "host", "", "Hostname or domain name of the node (e.g. mac-mini.local or 10.0.0.9)")
	setCmd.Flags().StringVar(&flagIP, "ip", "", "IP address of the node")
	setCmd.Flags().StringVar(&flagTags, "tags", "", "Comma-separated list of tags (e.g. 'home,desktop,m4')")

	kickCmd := &cobra.Command{
		Use:   "kick <node-name>",
		Short: "Revoke authorization credentials and remove node from the mesh",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			nodeName := args[0]
			repo, err := config.NewNodeRepository("")
			if err != nil {
				return err
			}
			node, err := repo.GetNode(nodeName)
			if err != nil {
				return fmt.Errorf("node '%s' not found: %w", nodeName, err)
			}

			// 1. If local server is running, revoke locally
			authStore, authErr := auth.NewAuthStore("")
			if authErr == nil && authStore != nil {
				_ = authStore.RevokeClient(node.Name)
				_ = authStore.RevokeClient(node.ID)
			}

			// 2. Notify remote node if reachable
			meshGw := mesh.NewMeshGateway()
			defer func() { _ = meshGw.Close() }()
			cli := client.NewMeshClient(meshGw)

			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			defer cancel()
			_ = cli.RevokeClient(ctx, node, node.Name)

			// 3. Remove from repository
			if err := repo.DeleteNode(nodeName); err != nil {
				return fmt.Errorf("failed to delete node from repository: %w", err)
			}

			if flagJSON {
				return presenter.PrintJSON(os.Stdout, map[string]any{"success": true, "kicked": nodeName})
			}
			fmt.Printf("✓ Kicked node '%s': token revoked and node removed from local mesh registry.\n", nodeName)
			return nil
		},
	}

	var sshUser string
	var sshPort uint16

	sshCmd := &cobra.Command{
		Use:   "ssh <[user@]node-name> [flags] [-- <ssh-arguments...>]",
		Short: "Connect to a mesh node via encrypted SSH tunnel",
		Long: `Connect directly to a mesh node using OpenSSH through the encrypted WireGuard mesh tunnel.
Traverses NAT, firewalls, and works without open public ports.

Examples:
  kizuna node ssh worker-1
  kizuna node ssh root@worker-1
  kizuna node ssh worker-1 -u root
  kizuna node ssh worker-1 -- uptime
  kizuna node ssh worker-1 -- -L 8080:localhost:8080`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rawTarget := args[0]
			var extraArgs []string
			if len(args) > 1 {
				extraArgs = args[1:]
			}

			user := sshUser
			nodeName := rawTarget
			if strings.Contains(rawTarget, "@") {
				parts := strings.SplitN(rawTarget, "@", 2)
				user = parts[0]
				nodeName = parts[1]
			}

			repo, err := config.NewNodeRepository("")
			if err != nil {
				return err
			}
			node, err := repo.GetNode(nodeName)
			if err != nil {
				return fmt.Errorf("node '%s' not found: %w", nodeName, err)
			}

			sshBin, err := exec.LookPath("ssh")
			if err != nil {
				return fmt.Errorf("ssh binary not found in PATH: please install openssh-client or use 'kizuna tunnel %d -n %s'", sshPort, nodeName)
			}

			exePath, err := os.Executable()
			if err != nil || exePath == "" {
				exePath = "kizuna"
			}

			// Build proxy command that tunnels stdio to the remote mesh port
			proxyCmdStr := fmt.Sprintf("%q node proxy %s %d", exePath, node.Name, sshPort)

			sshArgs := []string{
				"-o", "ProxyCommand=" + proxyCmdStr,
				"-o", "StrictHostKeyChecking=accept-new",
			}

			target := node.Name
			if user != "" {
				target = fmt.Sprintf("%s@%s", user, node.Name)
			}
			sshArgs = append(sshArgs, target)
			sshArgs = append(sshArgs, extraArgs...)

			c := exec.Command(sshBin, sshArgs...)
			c.Stdin = os.Stdin
			c.Stdout = os.Stdout
			c.Stderr = os.Stderr

			if err := c.Run(); err != nil {
				if exitErr, ok := err.(*exec.ExitError); ok {
					os.Exit(exitErr.ExitCode())
				}
				return err
			}
			return nil
		},
	}
	sshCmd.Flags().StringVarP(&sshUser, "user", "u", "", "SSH remote username")
	sshCmd.Flags().Uint16VarP(&sshPort, "port", "p", 22, "SSH remote port (default 22)")

	proxyCmd := &cobra.Command{
		Use:    "proxy <node-name> [port]",
		Short:  "Pipe stdin/stdout directly to a remote mesh node port (for SSH ProxyCommand)",
		Hidden: true,
		Args:   cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			nodeName := args[0]
			var port uint16 = 22
			if len(args) > 1 {
				_, _ = fmt.Sscanf(args[1], "%d", &port)
			}

			repo, err := config.NewNodeRepository("")
			if err != nil {
				return err
			}
			node, err := repo.GetNode(nodeName)
			if err != nil {
				return fmt.Errorf("node '%s' not found: %w", nodeName, err)
			}

			meshGw := mesh.NewMeshGateway()
			defer func() { _ = meshGw.Close() }()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			conn, err := meshGw.Dial(ctx, node.Addr, port)
			if err != nil {
				return fmt.Errorf("failed to dial node '%s' on port %d: %w", nodeName, port, err)
			}
			defer func() { _ = conn.Close() }()

			errCh := make(chan error, 2)
			go func() {
				_, err := io.Copy(conn, os.Stdin)
				if cw, ok := conn.(interface{ CloseWrite() error }); ok {
					_ = cw.CloseWrite()
				}
				errCh <- err
			}()
			go func() {
				_, err := io.Copy(os.Stdout, conn)
				errCh <- err
			}()

			select {
			case <-ctx.Done():
				return ctx.Err()
			case err := <-errCh:
				if err == io.EOF {
					return nil
				}
				return err
			}
		},
	}

	nodeCmd.AddCommand(addCmd, listCmd, rmCmd, tagCmd, setCmd, kickCmd, sshCmd, proxyCmd)
	return nodeCmd
}

func parseTags(args []string) []string {
	var result []string
	seen := make(map[string]bool)
	for _, a := range args {
		parts := strings.Split(a, ",")
		for _, p := range parts {
			tag := strings.TrimSpace(p)
			if tag != "" && !seen[tag] {
				seen[tag] = true
				result = append(result, tag)
			}
		}
	}
	return result
}

func listNodes() error {
	repo, err := config.NewNodeRepository("")
	if err != nil {
		return err
	}
	nodes, err := repo.ListNodes()
	if err != nil {
		return err
	}

	// 1. Check if local daemon is running and has live mesh members
	var meshNodes []*entity.Node
	clientHTTP := &http.Client{Timeout: 800 * time.Millisecond}
	resp, httpErr := clientHTTP.Get("http://127.0.0.1:19800/api/v1/node/members")
	if httpErr == nil && resp.StatusCode == http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		var gm []*entity.Node
		if json.NewDecoder(resp.Body).Decode(&gm) == nil && len(gm) > 0 {
			meshNodes = gm
		}
	}

	var finalNodes []*entity.Node
	if len(meshNodes) > 0 {
		finalNodes = meshNodes
	} else {
		meshGw := mesh.NewMeshGateway()
		defer func() { _ = meshGw.Close() }()
		cli := client.NewMeshClient(meshGw)

		var wg sync.WaitGroup
		probed := make([]*entity.Node, len(nodes))

		for i, n := range nodes {
			wg.Add(1)
			go func(idx int, target *entity.Node) {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
				defer cancel()

				nCopy := *target
				upNode, _, err := cli.GetStatus(ctx, target)
				if err != nil {
					if os.Getenv("KIZUNA_DEBUG") != "" {
						fmt.Fprintf(os.Stderr, "DEBUG GetStatus error: %v\n", err)
					}
					nCopy.IsOnline = false
					nCopy.Status = "dead"
					nCopy.GossipState = entity.GossipStateDead
					probed[idx] = &nCopy
				} else {
					if upNode != nil {
						upNode.IsOnline = true
						if upNode.Status == "" {
							upNode.Status = "alive"
						}
						upNode.GossipState = entity.GossipStateAlive
						probed[idx] = upNode
					} else {
						nCopy.IsOnline = true
						nCopy.Status = "alive"
						nCopy.GossipState = entity.GossipStateAlive
						probed[idx] = &nCopy
					}
				}
			}(i, n)
		}
		wg.Wait()
		finalNodes = probed
	}

	// Guarantee Status is populated on all returned nodes
	for _, n := range finalNodes {
		if n.Status == "" {
			if n.GossipState != "" {
				n.Status = string(n.GossipState)
			} else if n.IsOnline {
				n.Status = "alive"
			} else {
				n.Status = "offline"
			}
		}
	}

	if flagJSON {
		return presenter.PrintJSON(os.Stdout, finalNodes)
	}

	ui := presenter.NewUI("")
	fmt.Println(ui.RenderNodeTable(finalNodes))
	return nil
}

func newDeployCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "deploy [service-name]",
		Short: "Deploy configured workloads to target mesh node",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfgPath := flagConfig
			if cfgPath == "" {
				var err error
				cfgPath, err = config.FindConfigFile()
				if err != nil {
					return err
				}
			}

			project, err := config.LoadProjectWithEnv(cfgPath, flagEnv)
			if err != nil {
				return err
			}

			// Validate configuration before deployment
			issues := config.ValidateProject(project, filepath.Dir(cfgPath))
			hasErrors := false
			for _, issue := range issues {
				if issue.Severity == config.SeverityError {
					hasErrors = true
				}
			}
			if hasErrors {
				if flagJSON {
					return presenter.PrintJSON(os.Stdout, map[string]any{
						"success": false,
						"error":   "configuration validation failed",
						"issues":  issues,
					})
				}
				return fmt.Errorf("configuration validation failed; fix errors or run 'kizuna check'")
			}

			targetSvc := ""
			if len(args) > 0 {
				targetSvc = args[0]
			}

			repo, err := config.NewNodeRepository("")
			if err != nil {
				return err
			}
			meshGw := mesh.NewMeshGateway()
			defer func() { _ = meshGw.Close() }()

			cli := client.NewMeshClient(meshGw)
			runner := workload.NewWorkloadRunner("")
			localStrat := strategy.NewLocalStrategy(runner)
			meshStrat := strategy.NewMeshStrategy(cli, repo)
			sshStrat := strategy.NewSSHStrategy()
			stratFactory := strategy.NewFactory(localStrat, meshStrat, sshStrat)
			caddyMgr := ingress.NewCaddyManager("")

			deployUC := usecase.NewDeployUseCase(repo, cli).
				WithStrategyResolver(stratFactory).
				WithIngressManager(caddyMgr)

			out := io.Writer(os.Stdout)
			if flagJSON {
				out = io.Discard
			}

			if err := deployUC.Execute(cmd.Context(), project, targetSvc, out); err != nil {
				if flagJSON {
					return presenter.PrintJSON(os.Stdout, map[string]any{"success": false, "error": err.Error()})
				}
				return err
			}

			if flagJSON {
				return presenter.PrintJSON(os.Stdout, map[string]any{
					"success":     true,
					"project":     project.Name,
					"environment": project.ActiveEnv,
					"target":      project.Target,
					"targets":     project.Targets,
				})
			}

			return nil
		},
	}
	return cmd
}

func newScaleCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "scale [service] <replicas>",
		Short: "Scale service replicas horizontally with automatic load balancing",
		Example: `  kizuna scale 3
  kizuna scale web 3
  kizuna scale web=3`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return fmt.Errorf("usage: kizuna scale [service] <replicas>")
			}

			cfgPath := flagConfig
			if cfgPath == "" {
				var err error
				cfgPath, err = config.FindConfigFile()
				if err != nil {
					return err
				}
			}

			project, err := config.LoadProjectWithEnv(cfgPath, flagEnv)
			if err != nil {
				return err
			}

			targetSvc := ""
			replicas := 1

			if len(args) == 1 {
				arg := args[0]
				if strings.Contains(arg, "=") {
					parts := strings.Split(arg, "=")
					targetSvc = parts[0]
					replicas, _ = strconv.Atoi(parts[1])
				} else if r, err := strconv.Atoi(arg); err == nil {
					replicas = r
				} else {
					targetSvc = arg
				}
			} else if len(args) >= 2 {
				targetSvc = args[0]
				replicas, _ = strconv.Atoi(args[1])
			}

			if replicas <= 0 {
				return fmt.Errorf("replica count must be at least 1, got %d", replicas)
			}

			repo, err := config.NewNodeRepository("")
			if err != nil {
				return err
			}
			meshGw := mesh.NewMeshGateway()
			defer func() { _ = meshGw.Close() }()

			cli := client.NewMeshClient(meshGw)
			runner := workload.NewWorkloadRunner("")
			localStrat := strategy.NewLocalStrategy(runner)
			meshStrat := strategy.NewMeshStrategy(cli, repo)
			sshStrat := strategy.NewSSHStrategy()
			stratFactory := strategy.NewFactory(localStrat, meshStrat, sshStrat)
			caddyMgr := ingress.NewCaddyManager("")

			deployUC := usecase.NewDeployUseCase(repo, cli).
				WithStrategyResolver(stratFactory).
				WithIngressManager(caddyMgr)

			scaleUC := usecase.NewScaleUseCase(deployUC)

			out := io.Writer(os.Stdout)
			if flagJSON {
				out = io.Discard
			}

			if err := scaleUC.Execute(cmd.Context(), project, targetSvc, replicas, out); err != nil {
				if flagJSON {
					return presenter.PrintJSON(os.Stdout, map[string]any{"success": false, "error": err.Error()})
				}
				return err
			}

			if flagJSON {
				upstreams := []string{}
				if s, ok := project.Services[targetSvc]; ok && s.Ingress != nil {
					upstreams = s.Ingress.Upstreams
				}
				return presenter.PrintJSON(os.Stdout, map[string]any{
					"success":   true,
					"service":   targetSvc,
					"replicas":  replicas,
					"upstreams": upstreams,
				})
			}
			return nil
		},
	}
	return cmd
}

func newRollbackCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rollback [service]",
		Short: "Roll back a workload to its previous stable revision",
		Example: `  kizuna rollback
  kizuna rollback web`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfgPath := flagConfig
			if cfgPath == "" {
				var err error
				cfgPath, err = config.FindConfigFile()
				if err != nil {
					return err
				}
			}

			project, err := config.LoadProjectWithEnv(cfgPath, flagEnv)
			if err != nil {
				return err
			}

			targetSvc := ""
			if len(args) > 0 {
				targetSvc = args[0]
			}

			repo, err := config.NewNodeRepository("")
			if err != nil {
				return err
			}
			meshGw := mesh.NewMeshGateway()
			defer func() { _ = meshGw.Close() }()

			cli := client.NewMeshClient(meshGw)
			runner := workload.NewWorkloadRunner("")
			localStrat := strategy.NewLocalStrategy(runner)
			meshStrat := strategy.NewMeshStrategy(cli, repo)
			sshStrat := strategy.NewSSHStrategy()
			stratFactory := strategy.NewFactory(localStrat, meshStrat, sshStrat)
			caddyMgr := ingress.NewCaddyManager("")

			deployUC := usecase.NewDeployUseCase(repo, cli).
				WithStrategyResolver(stratFactory).
				WithIngressManager(caddyMgr)

			rollbackUC := usecase.NewRollbackUseCase(deployUC, "")

			out := io.Writer(os.Stdout)
			if flagJSON {
				out = io.Discard
			}

			targetRev, err := rollbackUC.Execute(cmd.Context(), project, targetSvc, out)
			if err != nil {
				if flagJSON {
					return presenter.PrintJSON(os.Stdout, map[string]any{"success": false, "error": err.Error()})
				}
				return err
			}

			if flagJSON {
				return presenter.PrintJSON(os.Stdout, map[string]any{
					"success":        true,
					"service":        targetSvc,
					"rolled_back_to": targetRev.Revision,
				})
			}
			return nil
		},
	}
	return cmd
}

func newDashboardCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "dashboard",
		Short: "Launch interactive real-time Charm Bubble Tea TUI dashboard",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDashboard()
		},
	}
}

func runDashboard() error {
	repo, err := config.NewNodeRepository("")
	var nodes []*entity.Node
	if err == nil {
		nodes, _ = repo.ListNodes()
	}

	themeName := ""
	var configuredServices []*entity.Service
	cfgPath := flagConfig
	if cfgPath == "" {
		cfgPath, _ = config.FindConfigFile()
	}
	if cfgPath != "" {
		if proj, err := config.LoadProjectWithEnv(cfgPath, flagEnv); err == nil {
			if proj.Theme != "" {
				themeName = proj.Theme
			}
			for _, svc := range proj.Services {
				configuredServices = append(configuredServices, svc)
			}
		}
	}
	activeTheme := entity.ResolveTheme(themeName, nil)

	meshGw := mesh.NewMeshGateway()
	defer func() { _ = meshGw.Close() }()
	cli := client.NewMeshClient(meshGw)

	model := presenter.NewDashboardWithOptions(presenter.DashboardOptions{
		Nodes:              nodes,
		ConfiguredServices: configuredServices,
		Client:             cli,
		Theme:              activeTheme,
		ActiveEnv:          flagEnv,
	})

	p := tea.NewProgram(model, tea.WithAltScreen())
	_, err = p.Run()
	return err
}

func newStatusCmd() *cobra.Command {
	var interactive bool
	var watch bool

	cmd := &cobra.Command{
		Use:   "status [service-name]",
		Short: "Display live status of mesh nodes, local system telemetry, and workloads",
		RunE: func(cmd *cobra.Command, args []string) error {
			if interactive || watch {
				return runDashboard()
			}

			// 1. Collect local host telemetry immediately (pure Go, ~5ms)
			col := telemetry.NewCollector()
			colCtx, colCancel := context.WithTimeout(cmd.Context(), 1*time.Second)
			localMetrics, _ := col.Collect(colCtx)
			colCancel()

			// 2. Load nodes from repository
			repo, err := config.NewNodeRepository("")
			var nodes []*entity.Node
			if err == nil {
				nodes, _ = repo.ListNodes()
			}

			// If no remote nodes configured, register local machine as node
			if len(nodes) == 0 {
				nodes = []*entity.Node{
					{
						Name:      "local-node",
						Addr:      "127.0.0.1:19800",
						AuthToken: "kzn_local",
						IsOnline:  true,
						OS:        localMetrics.OS,
						Arch:      localMetrics.Arch,
					},
				}
			}

			// 3. Load configured workloads from project config
			themeName := ""
			var configuredServices []*entity.Service
			cfgPath := flagConfig
			if cfgPath == "" {
				cfgPath, _ = config.FindConfigFile()
			}
			if cfgPath != "" {
				if proj, err := config.LoadProjectWithEnv(cfgPath, flagEnv); err == nil {
					if proj.Theme != "" {
						themeName = proj.Theme
					}
					for _, svc := range proj.Services {
						configuredServices = append(configuredServices, svc)
					}
				}
			}

			// 4. Concurrently probe nodes with strict timeout so unreachable nodes never block the CLI!
			meshGw := mesh.NewMeshGateway()
			defer func() { _ = meshGw.Close() }()
			cli := client.NewMeshClient(meshGw)

			type nodeResult struct {
				index    int
				node     *entity.Node
				services []*entity.Service
				latency  time.Duration
				err      error
			}

			resChan := make(chan nodeResult, len(nodes))
			var wg sync.WaitGroup

			for i, n := range nodes {
				wg.Add(1)
				go func(idx int, target *entity.Node) {
					defer wg.Done()
					start := time.Now()
					probeCtx, probeCancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
					defer probeCancel()

					nCopy := *target
					upNode, svcs, err := cli.GetStatus(probeCtx, target)
					latency := time.Since(start)
					if err != nil {
						nCopy.IsOnline = false
						resChan <- nodeResult{index: idx, node: &nCopy, latency: latency, err: err}
					} else {
						if upNode != nil {
							upNode.IsOnline = true
							resChan <- nodeResult{index: idx, node: upNode, services: svcs, latency: latency}
						} else {
							nCopy.IsOnline = true
							resChan <- nodeResult{index: idx, node: &nCopy, services: svcs, latency: latency}
						}
					}
				}(i, n)
			}

			wg.Wait()
			close(resChan)

			probedNodes := make([]*entity.Node, len(nodes))
			var activeServices []*entity.Service
			activeServices = append(activeServices, configuredServices...)

			for res := range resChan {
				probedNodes[res.index] = res.node
				if len(res.services) > 0 {
					for _, s := range res.services {
						found := false
						for _, cs := range activeServices {
							if cs.Name == s.Name {
								cs.State = s.State
								found = true
								break
							}
						}
						if !found {
							activeServices = append(activeServices, s)
						}
					}
				}
			}

			if flagJSON {
				return presenter.PrintJSON(os.Stdout, map[string]any{
					"system":    localMetrics,
					"nodes":     probedNodes,
					"workloads": activeServices,
				})
			}

			ui := presenter.NewUI(themeName)
			fmt.Println()
			fmt.Println(ui.RenderSystemCard(localMetrics))
			fmt.Println()
			fmt.Println(ui.RenderNodeTable(probedNodes))
			if len(activeServices) > 0 {
				fmt.Println()
				fmt.Println(ui.RenderServicesTable(activeServices))
			}
			fmt.Println()
			fmt.Println(ui.MutedStyle.Render("💡 Tip: Run 'kizuna dashboard' (or 'kizuna status -i') to launch the real-time interactive TUI."))
			return nil
		},
	}

	cmd.Flags().BoolVarP(&interactive, "interactive", "i", false, "Launch interactive Bubble Tea TUI dashboard")
	cmd.Flags().BoolVarP(&watch, "watch", "w", false, "Watch status in interactive Bubble Tea TUI")
	return cmd
}

func newLogsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logs <service-name>",
		Short: "Stream real-time logs for a deployed service",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svcName := args[0]
			repo, err := config.NewNodeRepository("")
			if err != nil {
				return err
			}
			nodes, _ := repo.ListNodes()
			var targetNode *entity.Node
			if len(nodes) > 0 {
				targetNode = nodes[0]
			} else {
				targetNode = &entity.Node{
					Name:      "local-node",
					Addr:      "127.0.0.1",
					AuthToken: "kzn_local",
					IsOnline:  true,
				}
			}

			meshGw := mesh.NewMeshGateway()
			defer func() { _ = meshGw.Close() }()
			cli := client.NewMeshClient(meshGw)

			return cli.StreamLogs(context.Background(), targetNode, svcName, os.Stdout)
		},
	}
}

func newBackupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "backup <service-name>",
		Short: "Trigger a backup snapshot for a deployed service",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svcName := args[0]
			repo, err := config.NewNodeRepository("")
			if err != nil {
				return err
			}
			nodes, _ := repo.ListNodes()
			var targetNode *entity.Node
			if len(nodes) > 0 {
				targetNode = nodes[0]
			} else {
				targetNode = &entity.Node{
					Name:      "local-node",
					Addr:      "127.0.0.1",
					AuthToken: "kzn_local",
					IsOnline:  true,
				}
			}

			var backupCfg *entity.BackupConfig
			if cfgPath, err := config.FindConfigFile(); err == nil {
				if proj, err := config.LoadProjectWithEnv(cfgPath, flagEnv); err == nil {
					if svc, ok := proj.Services[svcName]; ok {
						backupCfg = svc.Backup
					}
				}
			}
			if backupCfg == nil {
				backupCfg = &entity.BackupConfig{
					Paths:   []string{"."},
					Storage: &entity.StorageConfig{Type: "local"},
				}
			}

			meshGw := mesh.NewMeshGateway()
			defer func() { _ = meshGw.Close() }()
			cli := client.NewMeshClient(meshGw)

			if !flagJSON {
				fmt.Printf("Triggering backup for service '%s' on node '%s'...\n", svcName, targetNode.Name)
			}

			record, err := cli.TriggerBackup(context.Background(), targetNode, svcName, backupCfg)
			if err != nil {
				if flagJSON {
					return presenter.PrintJSON(os.Stdout, map[string]any{"success": false, "error": err.Error()})
				}
				return err
			}

			if flagJSON {
				return presenter.PrintJSON(os.Stdout, map[string]any{"success": true, "record": record})
			}

			ui := presenter.NewUI("")
			fmt.Printf("✓ %s\n  File: %s\n  Size: %d bytes\n  Storage: %s\n",
				ui.SecondaryStyle.Bold(true).Render("Backup created successfully!"),
				record.Filename, record.Size, record.StorageType)
			return nil
		},
	}
}

func newTunnelCmd() *cobra.Command {
	var localPort uint16
	var targetNodeName string

	cmd := &cobra.Command{
		Use:   "tunnel <remote-port>",
		Short: "Forward a remote port from the mesh node to localhost",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var remotePort uint16
			_, err := fmt.Sscanf(args[0], "%d", &remotePort)
			if err != nil || remotePort == 0 {
				return fmt.Errorf("invalid remote port: %s", args[0])
			}
			if localPort == 0 {
				localPort = remotePort
			}

			repo, err := config.NewNodeRepository("")
			if err != nil {
				return err
			}
			var targetNode *entity.Node
			if targetNodeName != "" {
				targetNode, err = repo.GetNode(targetNodeName)
				if err != nil {
					return err
				}
			} else {
				nodes, _ := repo.ListNodes()
				if len(nodes) > 0 {
					targetNode = nodes[0]
				} else {
					targetNode = &entity.Node{
						Name: "local-node",
						Addr: "127.0.0.1",
					}
				}
			}

			meshGw := mesh.NewMeshGateway()
			defer func() { _ = meshGw.Close() }()

			ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", localPort))
			if err != nil {
				return fmt.Errorf("failed to bind local port %d: %w", localPort, err)
			}
			defer func() { _ = ln.Close() }()

			ui := presenter.NewUI("")
			fmt.Printf("✓ %s\n  Forwarding: 127.0.0.1:%d -> %s:%d\n  Press Ctrl+C to stop.\n",
				ui.SecondaryStyle.Bold(true).Render("Tunnel established!"),
				localPort, targetNode.Name, remotePort)

			for {
				localConn, err := ln.Accept()
				if err != nil {
					return err
				}
				go func(c net.Conn) {
					defer func() { _ = c.Close() }()
					remoteConn, err := meshGw.Dial(context.Background(), targetNode.Addr, remotePort)
					if err != nil {
						fmt.Printf("Failed to dial remote port: %v\n", err)
						return
					}
					defer func() { _ = remoteConn.Close() }()

					go func() { _, _ = io.Copy(remoteConn, c) }()
					_, _ = io.Copy(c, remoteConn)
				}(localConn)
			}
		},
	}
	cmd.Flags().Uint16VarP(&localPort, "local", "l", 0, "Local port to listen on (defaults to same as remote port)")
	cmd.Flags().StringVarP(&targetNodeName, "node", "n", "", "Target mesh node name")
	return cmd
}

func newInitCmd() *cobra.Command {
	var templateType string

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize a new kizuna.yaml configuration with multi-environment presets",
		RunE: func(cmd *cobra.Command, args []string) error {
			targetFile := "kizuna.yaml"
			if _, err := os.Stat(targetFile); err == nil {
				return fmt.Errorf("kizuna.yaml already exists in current directory")
			}

			// Smart template detection if type not explicitly given
			if templateType == "" {
				if _, err := os.Stat("docker-compose.yml"); err == nil {
					templateType = "compose"
				} else if _, err := os.Stat("compose.yaml"); err == nil {
					templateType = "compose"
				} else if _, err := os.Stat("Dockerfile"); err == nil {
					templateType = "docker"
				} else if _, err := os.Stat("package.json"); err == nil {
					templateType = "bun"
				} else if _, err := os.Stat("apps"); err == nil {
					templateType = "monorepo"
				} else {
					templateType = "docker"
				}
			}

			content := generateTemplateContent(templateType)
			if err := os.WriteFile(targetFile, []byte(content), 0644); err != nil {
				return err
			}

			if flagJSON {
				return presenter.PrintJSON(os.Stdout, map[string]any{"success": true, "file": targetFile, "template": templateType})
			}

			ui := presenter.NewUI("")
			fmt.Printf("✓ %s (template: %s)\n\n", ui.SecondaryStyle.Bold(true).Render("Created kizuna.yaml"), templateType)
			fmt.Println("Next steps:")
			fmt.Println("  1. Edit 'kizuna.yaml' to customize environments (dev, staging, prod)")
			fmt.Println("  2. Run 'kizuna check' to validate your configuration")
			fmt.Println("  3. Run 'kizuna deploy -e staging' to publish to a target environment!")
			return nil
		},
	}
	cmd.Flags().StringVarP(&templateType, "type", "t", "", "Template type: docker, compose, bun, monorepo")
	return cmd
}

func generateTemplateContent(templateType string) string {
	switch templateType {
	case "compose":
		return `# Kizuna Configuration (Docker Compose)
version: "1"
name: "my-compose-app"
target: "production-server"
theme: "tokyonight"

type: "compose"
compose_file: "docker-compose.yml"

ingress:
  provider: "caddy"
  domain: "app.example.com"
  auto_tls: true
  upstream_port: 8080

backup:
  paths:
    - "./data"
  schedule: "0 3 * * *"
  storage:
    type: "local"

environments:
  development:
    target: "localhost"
    ingress:
      domain: "dev.example.local"
      auto_tls: false
      upstream_port: 8080

  staging:
    target: "staging-server"
    ingress:
      domain: "staging.example.com"
      upstream_port: 8080
`
	case "bun", "node":
		return `# Kizuna Configuration (Bun / Node)
version: "1"
name: "my-web-app"
target: "production-server"
theme: "tokyonight"

type: "bun" # 'bun' or 'node'
build:
  local: "bun run build"
  artifact: "./dist"

deploy:
  command: "bun run preview --port 3000"
  env:
    PORT: "3000"
    NODE_ENV: "production"

ingress:
  provider: "caddy"
  domain: "app.example.com"
  auto_tls: true
  upstream_port: 3000

environments:
  development:
    target: "localhost"
    deploy:
      command: "bun run dev --port 3001"
      env:
        PORT: "3001"
        NODE_ENV: "development"
    ingress:
      domain: "dev.example.local"
      upstream_port: 3001

  staging:
    target: "staging-server"
    ingress:
      domain: "staging.example.com"
      upstream_port: 3000
`
	case "monorepo":
		return `# Kizuna Configuration (Monorepo)
version: "1"
name: "my-monorepo"
target: "production-server"
theme: "tokyonight"

services:
  web:
    root: "./apps/web"
    type: "bun"
    build:
      local: "bun run build"
      artifact: "./apps/web/dist"
    deploy:
      command: "bun run preview --port 3000"
    ingress:
      provider: "caddy"
      domain: "web.example.com"
      upstream_port: 3000

  api:
    root: "./apps/api"
    type: "docker"
    dockerfile: "Dockerfile"
    ports:
      - "8080:8080"
    ingress:
      provider: "caddy"
      domain: "api.example.com"
      upstream_port: 8080

environments:
  development:
    target: "localhost"
    services:
      web:
        ingress:
          domain: "web.example.local"
          upstream_port: 3000
      api:
        ports:
          - "8081:8080"
`
	default: // docker
		return `# Kizuna Configuration (Docker)
version: "1"
name: "my-docker-app"
target: "production-server"
theme: "catppuccin"

type: "docker"
dockerfile: "Dockerfile"
ports:
  - "3000:3000"

env:
  NODE_ENV: "production"
  PORT: "3000"

ingress:
  provider: "caddy"
  domain: "app.example.com"
  auto_tls: true
  upstream_port: 3000

backup:
  paths:
    - "."
  schedule: "0 2 * * *"
  storage:
    type: "local"

environments:
  development:
    target: "localhost"
    ports:
      - "3001:3000"
    env:
      NODE_ENV: "development"
    ingress:
      domain: "dev.example.local"
      upstream_port: 3001

  staging:
    target: "staging-server"
    ports:
      - "3002:3000"
    ingress:
      domain: "staging.example.com"
      upstream_port: 3002
`
	}
}

func newCheckCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Validate kizuna.yaml configuration file and environment overrides",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfgPath := flagConfig
			if cfgPath == "" {
				var err error
				cfgPath, err = config.FindConfigFile()
				if err != nil {
					return err
				}
			}

			project, err := config.LoadProjectWithEnv(cfgPath, flagEnv)
			if err != nil {
				if flagJSON {
					return presenter.PrintJSON(os.Stdout, map[string]any{"valid": false, "error": err.Error()})
				}
				return fmt.Errorf("syntax error: %w", err)
			}

			issues := config.ValidateProject(project, filepath.Dir(cfgPath))
			hasErrors := false
			for _, issue := range issues {
				if issue.Severity == config.SeverityError {
					hasErrors = true
				}
			}

			if flagJSON {
				return presenter.PrintJSON(os.Stdout, map[string]any{
					"valid":        !hasErrors,
					"project":      project.Name,
					"target":       project.Target,
					"targets":      project.Targets,
					"environment":  project.ActiveEnv,
					"environments": project.Environments,
					"services":     len(project.Services),
					"issues":       issues,
				})
			}

			ui := presenter.NewUI(project.Theme)

			fmt.Println(ui.PrimaryStyle.Bold(true).Render("🔍 PREFLIGHT CONFIGURATION CHECK"))
			fmt.Printf("Config File: %s\n\n", ui.MutedStyle.Render(cfgPath))

			envBadge := ui.EnvBadge(project.ActiveEnv)
			fmt.Printf("• Project Name:  %s\n", ui.BoldStyle.Render(project.Name))
			fmt.Printf("• Environment:   %s\n", envBadge)
			if len(project.Targets) > 1 {
				fmt.Printf("• Targets (Scale): %s (%d nodes)\n", ui.SecondaryStyle.Render(strings.Join(project.Targets, ", ")), len(project.Targets))
			} else {
				fmt.Printf("• Target Node:   %s\n", ui.SecondaryStyle.Render(project.Target))
			}
			fmt.Printf("• Active Theme:  %s\n", ui.PrimaryStyle.Render(project.Theme))
			fmt.Printf("• Services:      %d defined\n\n", len(project.Services))

			for _, issue := range issues {
				var badge string
				switch issue.Severity {
				case config.SeverityWarning:
					badge = ui.Badge("WARN", ui.Theme.Warning, "#000")
				case config.SeverityError:
					badge = ui.Badge("FAIL", ui.Theme.Danger, "#FFF")
				default:
					badge = ui.Badge("INFO", ui.Theme.Primary, "#FFF")
				}
				fmt.Printf(" %s  %-20s %s\n", badge, ui.MutedStyle.Render(issue.Field), issue.Message)
			}

			if hasErrors {
				fmt.Printf("\n%s\n", ui.DangerStyle.Bold(true).Render("❌ Configuration validation failed! Please fix the errors above."))
				return fmt.Errorf("validation errors found")
			}

			if len(issues) > 0 {
				fmt.Printf("\n✓ %s\n", ui.SecondaryStyle.Render(fmt.Sprintf("Validation passed with %d warning(s). Ready to deploy!", len(issues))))
			} else {
				fmt.Printf("\n✓ %s\n", ui.SecondaryStyle.Bold(true).Render("Validation passed! Configuration is healthy and ready for deployment."))
			}

			return nil
		},
	}
	return cmd
}

func newUpgradeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade kizuna to the latest release from GitHub",
		RunE: func(cmd *cobra.Command, args []string) error {
			mgr := updater.NewManager("", "")
			return mgr.Upgrade(cmd.Context(), version, os.Stdout)
		},
	}
}
