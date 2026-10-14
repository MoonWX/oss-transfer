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
	if *backupState != "" || *restoreState != "" {
		if *configPath == "" || *checkConfig || *checkReady || *backupState != "" && *restoreState != "" {
			return errors.New("choose --backup-state or --restore-state with --config and no other checks")
		}
		if *restoreState != "" {
			return state.RestoreOffline(filepath.Join(config.DataDirectory, "master.db"), "master", *restoreState)
		}
		return state.BackupOffline(filepath.Join(config.DataDirectory, "master.db"), "master", *backupState)
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
		return serve(ctx, config, ready)
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
func serve(ctx context.Context, config servicehost.Config, ready func() error) error {
	assets, err := webui.Assets()
	if err != nil {
		return fmt.Errorf("load console assets (use the release binary or build the frontend): %w", err)
	}
	db, err := state.Open(filepath.Join(config.DataDirectory, "master.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	handler, bootstrap, err := master.New(db, master.Options{DataDirectory: config.DataDirectory, Assets: webui.Handler(assets), AgentArtifactBase: config.AgentArtifactBase, AgentArtifactVersion: config.AgentArtifactVersion, AgentDockerImage: config.AgentDockerImage})
	if err != nil {
		return err
	}
	defer handler.Close()
	listener, err := net.Listen("tcp", config.Listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	handler.Start()
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 32 * 1024}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	fmt.Printf("Master listening on %s\n", listener.Addr())
	if bootstrap != "" {
		fmt.Printf("Open the console to complete setup. Setup token: %s\n", bootstrap)
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
