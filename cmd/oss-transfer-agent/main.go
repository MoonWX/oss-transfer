package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agent"
	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/servicehost"
	"github.com/MoonWX/oss-transfer/internal/softwareupdate"
	"github.com/MoonWX/oss-transfer/internal/state"
)

var version = "4.1.0-dev"
var commit = "unknown"
var builtAt = "unknown"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) == 3 && os.Args[1] == "__apply-update" {
		return softwareupdate.RunHelper(os.Args[2])
	}
	joinURL := flag.String("join-url", "", "Agent invitation link (first enrollment)")
	masterURL := flag.String("master-url", "", "Master HTTP or HTTPS origin (first enrollment)")
	joinToken := flag.String("join-token", os.Getenv("OSS_TRANSFER_JOIN_TOKEN"), "One-use enrollment token (first enrollment)")
	address := flag.String("address", "", "Node address or name")
	label := flag.String("label", "", "Node display name")
	data := flag.String("data-dir", "./data/agent", "Agent data directory")
	configPath := flag.String("config", "", "Service JSON configuration")
	checkConfig := flag.Bool("check-config", false, "Validate configuration without starting the service")
	backupMaintenance := flag.String("backup-maintenance", "", "Manage upgrade backups: list, prune or complete")
	backupDirectory := flag.String("backup-dir", "", "Managed upgrade backup directory")
	backupEntry := flag.String("backup-entry", "", "Backup directory name to mark complete")
	backupState := flag.String("backup-state", "", "Validate and snapshot a stopped database to a new file")
	restoreState := flag.String("restore-state", "", "Restore a snapshot into a stopped existing database")
	compactState := flag.String("compact-state", "", "Write a compact, verified copy of a stopped database to a new file")
	checkReady := flag.Bool("check-ready", false, "Verify an installer startup record")
	expectPID := flag.Int("expect-pid", 0, "Service manager PID for readiness verification")
	expectLaunch := flag.String("expect-launch-id", "", "Installer startup ID for readiness verification")
	enrollOnly := flag.Bool("enroll-only", false, "Enroll durably and exit without starting transfers")
	showVersion := flag.Bool("version", false, "Print build version")
	flag.Parse()
	if *showVersion {
		fmt.Printf("oss-transfer-agent %s (%s, built %s)\n", version, commit, builtAt)
		return nil
	}
	if *backupMaintenance != "" {
		invalid := false
		flag.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "backup-maintenance", "backup-dir", "backup-entry", "config":
			default:
				invalid = true
			}
		})
		if invalid {
			return errors.New("backup maintenance cannot be combined with other operations")
		}
		return softwareupdate.RunBackupMaintenance("agent", *configPath, *backupDirectory, *backupMaintenance, *backupEntry, os.Stdout)
	}
	if *backupDirectory != "" || *backupEntry != "" {
		return errors.New("--backup-dir and --backup-entry require --backup-maintenance")
	}
	var explicitJoinURL, explicitMasterURL, explicitJoinToken, explicitData bool
	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "join-url":
			explicitJoinURL = true
		case "master-url":
			explicitMasterURL = true
		case "join-token":
			explicitJoinToken = true
		case "data-dir":
			explicitData = true
		}
	})
	options := agent.Options{MasterURL: *masterURL, JoinToken: *joinToken, Address: *address, Label: *label}
	if explicitJoinURL {
		var err error
		options, err = withJoinURL(*joinURL, options, explicitMasterURL || explicitJoinToken)
		if err != nil {
			return err
		}
	}
	selectedData := *data
	if !explicitData && *configPath == "" {
		var err error
		selectedData, err = defaultAgentDataDirectory()
		if err != nil {
			return err
		}
	}
	directory, err := filepath.Abs(selectedData)
	if err != nil {
		return err
	}
	config := servicehost.Config{ServiceName: "oss-transfer-agent", DataDirectory: directory}
	if *configPath != "" {
		if explicitData {
			return errors.New("use the service configuration for the data directory")
		}
		config, err = servicehost.LoadConfig(*configPath, "agent")
		if err != nil {
			return err
		}
	}
	if *backupState != "" || *restoreState != "" || *compactState != "" {
		if *configPath == "" || *checkConfig || *checkReady || *enrollOnly || (*backupState != "" && *restoreState != "" || *backupState != "" && *compactState != "" || *restoreState != "" && *compactState != "") {
			return errors.New("choose one of --backup-state, --restore-state or --compact-state with --config and no other checks or enrollment")
		}
		if *compactState != "" {
			return compactAgentDatabase(filepath.Join(config.DataDirectory, "agent.db"), *compactState, os.Stderr)
		}
		if *restoreState != "" {
			return state.RestoreOffline(filepath.Join(config.DataDirectory, "agent.db"), "agent", *restoreState)
		}
		return state.BackupOfflineWithProgress(filepath.Join(config.DataDirectory, "agent.db"), "agent", *backupState, os.Stderr)
	}
	if *checkConfig {
		if *configPath == "" {
			return errors.New("--check-config requires --config")
		}
		return servicehost.InspectConfig(os.Stdout, config)
	}
	if *checkReady {
		return servicehost.CheckReady(config.ReadyFile, *expectLaunch, "agent", version, *expectPID)
	}
	if *enrollOnly {
		db, err := state.Open(filepath.Join(config.DataDirectory, "agent.db"))
		if err != nil {
			return err
		}
		defer db.Close()
		runtime, err := agent.New(db, options)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if err = runtime.Enroll(ctx); err != nil {
			return err
		}
		fmt.Printf("Agent enrolled: %s\n", runtime.Session().NodeID)
		return nil
	}
	return servicehost.Run(servicehost.Options{Name: config.ServiceName, Role: "agent", Version: version, ReadyFile: config.ReadyFile, LogFile: config.LogFile}, func(ctx context.Context, ready func() error) error {
		updater := softwareupdate.New(softwareupdate.Options{Role: "agent", Version: version, Commit: commit, DataDirectory: config.DataDirectory, ConfigPath: *configPath, ServiceName: config.ServiceName})
		return serve(ctx, config, options, updater, ready)
	})
}
func serve(ctx context.Context, config servicehost.Config, options agent.Options, updater softwareupdate.Controller, ready func() error) error {
	nodeOptions, err := config.NodeControl.Options()
	if err != nil {
		return err
	}
	db, err := state.Open(filepath.Join(config.DataDirectory, "agent.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	service, err := agentservice.New(db, agentservice.Options{SpoolDirectory: filepath.Join(config.DataDirectory, "spool"), NodeControl: nodeOptions, Updater: updater})
	if err != nil {
		return err
	}
	if n := len(service.TempRecovery.Retained); n > 0 {
		fmt.Fprintf(os.Stderr, "Temporary-file recovery retained %d files whose ownership or directory could not be verified\n", n)
	}
	if n := len(service.MPURecovery.Retained); n > 0 {
		fmt.Fprintf(os.Stderr, "Multipart recovery retained %d uploads whose ownership could not be verified\n", n)
	}
	defer func() {
		stop, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = service.Close(stop)
	}()
	options.Executor = service
	options.OnReady = ready
	var clusterOrigin string
	options.OnCLIAuth = func(token string) error {
		if err := servicehost.PublishCLILocalAuth("agent", clusterOrigin, token, config); err != nil {
			fmt.Fprintf(os.Stderr, "CLI local access publication: %v\n", err)
			return err
		}
		return nil
	}
	runner, err := agent.New(db, options)
	if err != nil {
		return err
	}
	clusterOrigin = runner.MasterURL()
	if err := servicehost.PublishCLIConnection("agent", runner.MasterURL(), config); err != nil {
		fmt.Fprintf(os.Stderr, "CLI connection discovery: %v\n", err)
	}
	err = runner.Run(ctx)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
