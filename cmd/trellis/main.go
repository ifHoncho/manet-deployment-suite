// Command trellis is the single Trellis binary. It runs as meshd, meshctl,
// webd or cotd depending on the first argument or the name it is invoked
// by (symlinks), as architecture §12.2 describes.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"meridian/trellis/internal/api"
	"meridian/trellis/internal/config"
	"meridian/trellis/internal/meshctl"
	"meridian/trellis/internal/meshd"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(run(os.Args))
}

func run(argv []string) int {
	prog := filepath.Base(argv[0])
	args := argv[1:]
	var sub string
	switch prog {
	case "meshd", "meshctl", "webd", "cotd":
		sub = prog
	default:
		if len(args) == 0 {
			usage()
			return 2
		}
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "meshd":
		return runMeshd(args)
	case "meshctl":
		return meshctl.Main(version, args, os.Stdout, os.Stderr)
	case "webd":
		fmt.Fprintln(os.Stderr, "webd (web UI and HTTP API) arrives in milestone M5; use meshctl for now")
		return 1
	case "cotd":
		fmt.Fprintln(os.Stderr, "cotd (ATAK CoT and Meshtastic bridge) arrives in milestone M6")
		return 1
	case "version", "--version", "-v":
		fmt.Println("trellis", version)
		return 0
	case "help", "--help", "-h":
		usage()
		return 0
	}
	fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n", sub)
	usage()
	return 2
}

func usage() {
	fmt.Fprintf(os.Stderr, `trellis %s — Meridian Trellis mesh node

Usage: trellis <subcommand> [flags]

  meshd     the privileged network daemon (run as root, under systemd)
  meshctl   operator CLI: status, radios, doctor, config validate, profile new
  webd      web UI (M5)
  cotd      ATAK / Meshtastic bridge (M6)
  version

The binary may also be installed as symlinks named meshd and meshctl.
`, version)
}

func runMeshd(args []string) int {
	fs := flag.NewFlagSet("meshd", flag.ContinueOnError)
	node := fs.String("node", config.DefaultNodePath, "node configuration file")
	profile := fs.String("profile", "", "mesh profile (default: the path in the node configuration)")
	stateDir := fs.String("state-dir", config.DefaultStateDir, "persistent state directory")
	runDir := fs.String("run-dir", config.DefaultRunDir, "runtime directory (sockets, generated configs)")
	socket := fs.String("socket", "", "API socket (default <run-dir>/meshd.sock)")
	logLevel := fs.String("log-level", "info", "debug, info, warn or error")
	logFormat := fs.String("log-format", "text", "text or json")
	interval := fs.Duration("interval", 15*time.Second, "slow reconcile interval")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "meshd is the daemon and takes no arguments (got %q).\n", strings.Join(fs.Args(), " "))
		fmt.Fprintln(os.Stderr, "For status use: sudo meshctl status. To restart the daemon: sudo systemctl restart trellis-meshd")
		return 2
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(strings.ToUpper(*logLevel))); err != nil {
		fmt.Fprintln(os.Stderr, "bad --log-level:", err)
		return 2
	}
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if *logFormat == "json" {
		h = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		// Under systemd the journal adds timestamps; keep lines compact.
		if os.Getenv("JOURNAL_STREAM") != "" {
			opts.ReplaceAttr = func(groups []string, a slog.Attr) slog.Attr {
				if a.Key == slog.TimeKey && len(groups) == 0 {
					return slog.Attr{}
				}
				return a
			}
		}
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	log := slog.New(h)
	slog.SetDefault(log)

	d, err := meshd.New(meshd.Options{
		Version: version, NodePath: *node, ProfilePath: *profile, StateDir: *stateDir, RunDir: *runDir,
		Socket: *socket, Log: log, Interval: *interval,
	})
	if err != nil {
		log.Error("meshd cannot start", "err", err)
		return 1
	}
	sock := *socket
	if sock == "" {
		sock = filepath.Join(*runDir, "meshd.sock")
	}
	serve := func(ctx context.Context, d *meshd.Daemon) error {
		srv := &api.Server{Provider: d, Log: log.With("component", "api"), Socket: sock}
		return srv.Serve(ctx)
	}
	if err := d.Run(context.Background(), serve); err != nil {
		log.Error("meshd exited with error", "err", err)
		return 1
	}
	return 0
}
