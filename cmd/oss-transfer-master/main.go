package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/MoonWX/oss-transfer/internal/master"
	"github.com/MoonWX/oss-transfer/internal/servicehost"
	"github.com/MoonWX/oss-transfer/internal/softwareupdate"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/webui"
)

var version = "4.1.0-dev"
var commit = "unknown"
var builtAt = "unknown"
var agentArtifactBase string
var agentArtifactVersion string
var agentDockerImage string

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
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		return migrate(context.Background(), os.Args[2:], os.Stdout, os.Stderr)
	}
	listen := flag.String("listen", ":1685", "HTTP listen address")
	data := flag.String("data-dir", "./data/master", "Master data directory")
	artifactBase := flag.String("agent-artifact-base", "", "Base URL for native Agent release downloads")
	artifactVersion := flag.String("agent-artifact-version", "", "Fixed Agent release version")
	agentImage := flag.String("agent-docker-image", "", "Agent container image")
	configPath := flag.String("config", "", "Service JSON configuration")
	checkConfig := flag.Bool("check-config", false, "Validate configuration without starting the service")
	backupState := flag.String("backup-state", "", "Validate and snapshot a stopped database to a new file")
	restoreState := flag.String("restore-state", "", "Restore a snapshot into a stopped existing database")
	compactState := flag.String("compact-state", "", "Write a compact, verified copy of a stopped database to a new file")
	resetPassword := flag.Bool("reset-admin-password", false, "Reset an administrator password while the Master is stopped")
	resetUsername := flag.String("username", "", "Administrator account to reset (required only if there is more than one)")
	checkReady := flag.Bool("check-ready", false, "Verify an installer startup record")
	expectPID := flag.Int("expect-pid", 0, "Service manager PID for readiness verification")
	expectLaunch := flag.String("expect-launch-id", "", "Installer startup ID for readiness verification")
	showVersion := flag.Bool("version", false, "Print build version")
	flag.Parse()
	if *showVersion {
		fmt.Printf("oss-transfer-master %s (%s, built %s)\n", version, commit, builtAt)
		return nil
	}
	directory, err := filepath.Abs(*data)
	if err != nil {
		return err
	}
	config := servicehost.Config{ServiceName: "oss-transfer-master", DataDirectory: directory, Listen: *listen, AgentArtifactBase: *artifactBase, AgentArtifactVersion: *artifactVersion, AgentDockerImage: *agentImage}
	if *configPath != "" {
		explicitRuntime := false
		flag.Visit(func(f *flag.Flag) {
			if f.Name == "listen" || f.Name == "data-dir" || f.Name == "agent-artifact-base" || f.Name == "agent-artifact-version" || f.Name == "agent-docker-image" {
				explicitRuntime = true
			}
		})
		if explicitRuntime {
			return errors.New("use the service configuration for runtime settings")
		}
		config, err = servicehost.LoadConfig(*configPath, "master")
		if err != nil {
			return err
		}
	}
	if err := configureAgentArtifacts(&config); err != nil {
		return err
	}
	if *resetPassword {
		if *backupState != "" || *restoreState != "" || *compactState != "" || *checkConfig || *checkReady {
			return errors.New("--reset-admin-password cannot be combined with database or service checks")
		}
		return resetAdministrator(config.DataDirectory, *resetUsername)
	}
	if *resetUsername != "" {
		return errors.New("--username requires --reset-admin-password")
	}
	if *backupState != "" || *restoreState != "" || *compactState != "" {
		if *configPath == "" || *checkConfig || *checkReady || (*backupState != "" && *restoreState != "" || *backupState != "" && *compactState != "" || *restoreState != "" && *compactState != "") {
			return errors.New("choose one of --backup-state, --restore-state or --compact-state with --config and no other checks")
		}
		if *compactState != "" {
			return state.CompactOfflineWithProgress(filepath.Join(config.DataDirectory, "master.db"), "master", *compactState, os.Stderr)
		}
		if *restoreState != "" {
			return state.RestoreOffline(filepath.Join(config.DataDirectory, "master.db"), "master", *restoreState)
		}
		return state.BackupOfflineWithProgress(filepath.Join(config.DataDirectory, "master.db"), "master", *backupState, os.Stderr)
	}
	if *checkConfig {
		if *configPath == "" {
			return errors.New("--check-config requires --config")
		}
		return servicehost.InspectConfig(os.Stdout, config)
	}
	if *checkReady {
		return servicehost.CheckReady(config.ReadyFile, *expectLaunch, "master", version, *expectPID)
	}
	return servicehost.Run(servicehost.Options{Name: config.ServiceName, Role: "master", Version: version, ReadyFile: config.ReadyFile, LogFile: config.LogFile}, func(ctx context.Context, ready func() error) error {
		updater := softwareupdate.New(softwareupdate.Options{Role: "master", Version: version, Commit: commit, DataDirectory: config.DataDirectory, ConfigPath: *configPath, ServiceName: config.ServiceName})
		return serve(ctx, config, updater, ready)
	})
}

// A published binary carries its matching Agent release. Configured mirrors
// remain explicit; never combine half of a custom URL/version with a default.
func configureAgentArtifacts(config *servicehost.Config) error {
	if config.AgentArtifactBase == "" && config.AgentArtifactVersion == "" {
		config.AgentArtifactBase = agentArtifactBase
		config.AgentArtifactVersion = agentArtifactVersion
	}
	if config.AgentDockerImage == "" {
		config.AgentDockerImage = agentDockerImage
	}
	return master.ValidateAgentArtifacts(master.Options{AgentArtifactBase: config.AgentArtifactBase, AgentArtifactVersion: config.AgentArtifactVersion, AgentDockerImage: config.AgentDockerImage})
}
func serve(ctx context.Context, config servicehost.Config, updater softwareupdate.Controller, ready func() error) error {
	assets, err := webui.Assets()
	if err != nil {
		return fmt.Errorf("load console assets (use the release binary or build the frontend): %w", err)
	}
	// Bind before creating credentials so an occupied port cannot consume the
	// one-time password output.
	listener, err := net.Listen("tcp", config.Listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	db, err := state.Open(filepath.Join(config.DataDirectory, "master.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	handler, initialPassword, err := master.New(db, master.Options{DataDirectory: config.DataDirectory, Assets: webui.Handler(assets), AgentArtifactBase: config.AgentArtifactBase, AgentArtifactVersion: config.AgentArtifactVersion, AgentDockerImage: config.AgentDockerImage, Updater: updater})
	if err != nil {
		return err
	}
	defer handler.Close()
	handler.Start()
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 32 * 1024}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	fmt.Printf("Master listening on %s\n", listener.Addr())
	if initialPassword != "" {
		printInitialPassword("admin", initialPassword)
	}
	if err := ready(); err != nil {
		_ = server.Close()
		return err
	}
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		_ = handler.Close()
		shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			return err
		}
		return nil
	}
}

func printInitialPassword(username, password string) {
	fmt.Printf("Initial administrator: %s\nInitial password: %s\n", username, password)
}

func resetAdministrator(directory, username string) error {
	path := filepath.Join(directory, "master.db")
	if info, err := os.Stat(path); err != nil {
		return err
	} else if !info.Mode().IsRegular() {
		return errors.New("Master database is not a regular file")
	}
	db, err := state.Open(path)
	if err != nil {
		return fmt.Errorf("stop the Master before resetting its password: %w", err)
	}
	defer db.Close()
	account, password, err := master.ResetAdministratorPassword(db, username)
	if err != nil {
		return err
	}
	printInitialPassword(account, password)
	return nil
}
