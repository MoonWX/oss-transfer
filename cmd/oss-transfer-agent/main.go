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
	masterURL := flag.String("master-url", "", "Master HTTP or HTTPS origin (first enrollment)")
	joinToken := flag.String("join-token", os.Getenv("OSS_TRANSFER_JOIN_TOKEN"), "One-use enrollment token (first enrollment)")
	address := flag.String("address", "", "Node address or name")
	label := flag.String("label", "", "Node display name")
	data := flag.String("data-dir", "./data/agent", "Agent data directory")
	configPath := flag.String("config", "", "Service JSON configuration")
	checkConfig := flag.Bool("check-config", false, "Validate configuration without starting the service")
	backupState := flag.String("backup-state", "", "Validate and snapshot a stopped database to a new file")
	restoreState := flag.String("restore-state", "", "Restore a snapshot into a stopped existing database")
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
	directory, err := filepath.Abs(*data)
	if err != nil {
		return err
	}
	config := servicehost.Config{ServiceName: "oss-transfer-agent", DataDirectory: directory}
	if *configPath != "" {
		explicitData := false
		flag.Visit(func(f *flag.Flag) {
			if f.Name == "data-dir" {
				explicitData = true
			}
		})
		if explicitData {
			return errors.New("use the service configuration for the data directory")
		}
		config, err = servicehost.LoadConfig(*configPath, "agent")
		if err != nil {
			return err
		}
	}
	if *backupState != "" || *restoreState != "" {
		if *configPath == "" || *checkConfig || *checkReady || *enrollOnly || *backupState != "" && *restoreState != "" {
			return errors.New("choose --backup-state or --restore-state with --config and no other checks or enrollment")
		}
		if *restoreState != "" {
			return state.RestoreOffline(filepath.Join(config.DataDirectory, "agent.db"), "agent", *restoreState)
		}
		return state.BackupOffline(filepath.Join(config.DataDirectory, "agent.db"), "agent", *backupState)
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
	options := agent.Options{MasterURL: *masterURL, JoinToken: *joinToken, Address: *address, Label: *label}
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
		return serve(ctx, config, options, ready)
	})
}
func serve(ctx context.Context, config servicehost.Config, options agent.Options, ready func() error) error {
	nodeOptions, err := config.NodeControl.Options()
	if err != nil {
		return err
	}
	db, err := state.Open(filepath.Join(config.DataDirectory, "agent.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	service, err := agentservice.New(db, agentservice.Options{SpoolDirectory: filepath.Join(config.DataDirectory, "spool"), NodeControl: nodeOptions})
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
	runner, err := agent.New(db, options)
	if err != nil {
		return err
	}
	err = runner.Run(ctx)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
