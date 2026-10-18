package app

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/prepaser/mytracker/internal/atomicfile"
	"github.com/prepaser/mytracker/internal/backup"
	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/gpx"
	"github.com/prepaser/mytracker/internal/importzip"
	"github.com/prepaser/mytracker/internal/privacy"
	"github.com/prepaser/mytracker/internal/slogx"
	"github.com/prepaser/mytracker/internal/store"
)

func Run(args []string) int {
	command := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command = args[0]
		args = args[1:]
	}
	fs := flag.NewFlagSet("mytracker", flag.ContinueOnError)
	cfgPath := fs.String("config", config.DefaultConfigPath, "config file path")
	purgeBackups := fs.Bool("purge-backups", false, "remove old backups after privacy scrub")
	auditProofs := fs.Bool("proofs", false, "cryptographically verify export proofs")
	outputFormat := fs.String("format", "json", "output format: json or text")
	gpxUser := fs.String("user", "", "location user")
	gpxDevice := fs.String("device", "", "location device")
	gpxFrom := fs.Int64("from", 0, "start Unix timestamp")
	gpxTo := fs.Int64("to", 0, "end Unix timestamp")
	if err := fs.Parse(configFlagFirst(args)); err != nil {
		return 2
	}
	switch command {
	case "serve", "doctor", "backup", "restore", "privacy-scrub", "import-gpx", "export-gpx", "verify-export", "verify-backup":
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", command)
		return 2
	}
	if command == "serve" || command == "doctor" || command == "backup" {
		if fs.NArg() != 0 {
			fmt.Fprintf(os.Stderr, "usage: mytracker %s [-config path]\n", command)
			return 2
		}
	}
	if command == "restore" {
		if fs.NArg() != 1 {
			fmt.Fprintln(os.Stderr, "usage: mytracker restore <backup.mtbackup> [-config path]")
			return 2
		}
	}
	if command == "privacy-scrub" {
		if fs.NArg() != 0 {
			fmt.Fprintln(os.Stderr, "usage: mytracker privacy-scrub [--purge-backups] [-config path]")
			return 2
		}
	}
	if command == "import-gpx" {
		if fs.NArg() != 1 || *gpxUser == "" || *gpxDevice == "" {
			fmt.Fprintln(os.Stderr, "usage: mytracker import-gpx <file> --user <user> --device <device> [-config path]")
			return 2
		}
		if err := gpx.ValidateIdentity(*gpxUser, *gpxDevice); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	}
	if command == "export-gpx" {
		if fs.NArg() != 1 {
			fmt.Fprintln(os.Stderr, "usage: mytracker export-gpx <file> [--from timestamp --to timestamp --user user --device device] [-config path]")
			return 2
		}
		if *gpxFrom < 0 || *gpxTo < 0 {
			fmt.Fprintln(os.Stderr, "from and to must be nonnegative Unix timestamps")
			return 2
		}
		if *gpxFrom > 0 && *gpxTo > 0 && *gpxFrom > *gpxTo {
			fmt.Fprintln(os.Stderr, "from must not be after to")
			return 2
		}
	}
	if command == "verify-export" || command == "verify-backup" {
		if fs.NArg() != 1 {
			fmt.Fprintf(os.Stderr, "usage: mytracker %s <file>\n", command)
			return 2
		}
		if err := backup.ScavengeTemp(); err != nil {
			slog.Error("verification temporary cleanup", "err", err)
			return 1
		}
		var result any
		var err error
		if command == "verify-export" {
			if *auditProofs {
				if *outputFormat != "json" && *outputFormat != "text" {
					fmt.Fprintln(os.Stderr, "format must be json or text")
					return 2
				}
				auditCfg, cfgErr := loadAuditConfig(*cfgPath)
				if cfgErr != nil {
					slog.Error("audit config", "err", cfgErr)
					return 1
				}
				report, auditErr := importzip.AuditFileTo(context.Background(), fs.Arg(0), auditCfg, commandOutput, *outputFormat)
				if auditErr != nil {
					slog.Error("verify", "kind", "export-proofs", "err", auditErr)
					return 1
				}
				return importzip.AuditExitCode(report)
			}
			result, err = importzip.VerifyFile(fs.Arg(0))
		} else {
			if err := backup.VerifyToWriter(context.Background(), fs.Arg(0), commandOutput); err != nil {
				slog.Error("verify", "kind", command, "err", err)
				return 1
			}
			return 0
		}
		if err != nil {
			slog.Error("verify", "kind", command, "err", err)
			return 1
		}
		if err := writeCommandJSON(result); err != nil {
			slog.Error("write output", "err", err)
			return 1
		}
		return 0
	}

	var cfg *config.Config
	var initMsg string
	var err error
	if command == "doctor" {
		cfg, err = config.Read(*cfgPath)
	} else {
		cfg, initMsg, err = config.Load(*cfgPath)
	}
	if err != nil {
		slog.Error("config load", "err", err)
		return 1
	}
	slogx.Init(cfg.LogLevel)
	if initMsg != "" {
		slog.Info(initMsg)
	}
	if addr := strings.TrimSpace(os.Getenv("MYTRACKER_LISTEN_ADDR")); addr != "" {
		cfg.ListenAddr = addr
	}
	if err := cfg.Validate(); err != nil {
		slog.Error("config validate", "err", err)
		return 1
	}
	warnListenAddr(cfg)
	if command == "doctor" {
		report, err := inspectInstance(cfg)
		if err != nil {
			slog.Error("doctor", "err", err)
			return 1
		}
		if err := writeCommandJSON(report); err != nil {
			slog.Error("write output", "err", err)
			return 1
		}
		return 0
	}
	if err := cfg.EnsureDirs(); err != nil {
		slog.Error("ensure dirs", "err", err)
		return 1
	}
	exclusive := command == "restore" || command == "privacy-scrub"
	lock, err := lockDataDir(cfg.DataDir, exclusive)
	if err != nil {
		slog.Error("data directory is in use", "err", err)
		return 1
	}
	defer func() { _ = lock.Close() }()
	if command == "serve" {
		serverLock, err := lockFile(filepath.Join(cfg.DataDir, ".mytracker-server.lock"), true)
		if err != nil {
			slog.Error("server is already running", "err", err)
			return 1
		}
		defer func() { _ = serverLock.Close() }()
	}
	if err := recoverRestore(cfg, lock, exclusive); err != nil {
		slog.Error("restore recovery", "err", err)
		return 1
	}
	if command == "restore" {
		return runRestoreCommand(fs.Arg(0), cfg)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		slog.Error("db open", "err", err)
		return 1
	}
	closeStore := true
	defer func() {
		if closeStore {
			_ = st.Close()
		}
	}()
	_, err = loadSettings(cfg, st)
	if err != nil {
		slog.Error("settings load", "err", err)
		return 1
	}
	cfgMgr := config.NewManager(cfg, st)
	if command == "serve" {
		created, err := cfgMgr.Initialize()
		if err != nil {
			slog.Error("settings initialize", "err", err)
			return 1
		}
		cfg = cfgMgr.Snapshot()
		if created {
			slog.Warn("initial settings created; read and delete the initial password file", "web_login", cfg.LoginURL(), "initial_password_file", cfgMgr.InitialPasswordPath())
		}
	}
	switch command {
	case "import-gpx":
		f, err := os.Open(fs.Arg(0))
		if err != nil {
			slog.Error("GPX open", "err", err)
			return 1
		}
		info, err := f.Stat()
		if err != nil {
			_ = f.Close()
			slog.Error("GPX size", "err", err)
			return 1
		}
		if info.Size() > cfg.ImportMaxBytes {
			_ = f.Close()
			slog.Error("GPX exceeds import_max_bytes", "size", info.Size())
			return 1
		}
		counts, err := gpx.ImportContext(context.Background(), f, st, *gpxUser, *gpxDevice, cfg.StampRetries)
		_ = f.Close()
		if err != nil {
			slog.Error("GPX import", "err", err)
			return 1
		}
		if err := writeCommandJSON(map[string]int{"imported": counts.Imported, "skipped": counts.Skipped}); err != nil {
			slog.Error("write output", "err", err)
			return 1
		}
		return 0
	case "export-gpx":
		snap, err := st.BeginTrackSnapshot(store.TrackFilter{User: *gpxUser, Device: *gpxDevice, From: *gpxFrom, To: *gpxTo})
		if err != nil {
			slog.Error("GPX snapshot", "err", err)
			return 1
		}
		err = atomicfile.WriteFunc(fs.Arg(0), 0600, func(out *os.File) error {
			return gpx.Write(out, "mytracker", func(fn func([]store.Location) error) error { return snap.Iterate(500, fn) }, cfg.TripGapDuration())
		})
		if atomicfile.IsCommitted(err) {
			err = nil
		}
		_ = snap.Close()
		if err != nil {
			slog.Error("GPX export", "err", err)
			return 1
		}
		if err := writeCommandLine(fs.Arg(0)); err != nil {
			slog.Error("write output", "err", err)
			return 1
		}
		return 0
	case "backup":
		path, err := backup.Create(cfg, st)
		if err != nil {
			slog.Error("backup", "err", err)
			return 1
		}
		if err := writeCommandLine(path); err != nil {
			slog.Error("write output", "err", err)
			return 1
		}
		return 0
	case "privacy-scrub":
		if err := st.ResetTimelines(); err != nil {
			slog.Error("privacy scrub timelines", "err", err)
			return 1
		}
		if err := st.Vacuum(); err != nil {
			slog.Error("privacy scrub vacuum", "err", err)
			return 1
		}
		audit, err := st.AuditProofs(cfg.ProofsDir())
		if err != nil {
			slog.Error("privacy scrub proofs", "err", err)
			return 1
		}
		if err := store.RemoveOrphanProofs(cfg.ProofsDir(), audit.Orphan); err != nil {
			slog.Error("privacy scrub proofs", "err", err)
			return 1
		}
		for {
			n, err := st.ProcessProofGC(cfg.ProofsDir(), 1000)
			if err != nil {
				slog.Error("privacy scrub proof gc", "err", err)
				return 1
			}
			if n == 0 {
				break
			}
		}
		gc, err := st.ProofGCStatus()
		if err != nil || gc.Pending > 0 {
			slog.Error("privacy scrub proof cleanup pending", "pending", gc.Pending, "err", err)
			return 1
		}
		result := map[string]any{"ok": true, "orphan_proofs_removed": len(audit.Orphan)}
		if *purgeBackups {
			path, err := backup.CreateSafety(cfg, st)
			if err != nil {
				slog.Error("privacy scrub backup", "err", err)
				return 1
			}
			if err := privacy.RemoveBackupsExcept(cfg.BackupsDir(), path); err != nil {
				slog.Error("privacy scrub purge backups", "err", err)
				return 1
			}
			if err := removePrivacySafetyBackup(cfg.DataDir); err != nil {
				slog.Error("privacy scrub safety backup", "err", err)
				return 1
			}
			result["backup"] = path
		}
		if err := writeCommandJSON(result); err != nil {
			slog.Error("write output", "err", err)
			return 1
		}
		return 0
	case "serve":
	}

	serveCtx, stopServe := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopServe()
	closeStore = false
	code, stopped := serveManaged(serveCtx, cfg, cfgMgr, st)
	closeStore = stopped
	return code
}
