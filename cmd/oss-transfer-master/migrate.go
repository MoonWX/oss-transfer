package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/MoonWX/oss-transfer/internal/legacyimport"
)

type migrationPlan struct {
	Snapshots               []string                                   `json:"snapshots"`
	Destination             string                                     `json:"destination"`
	PublicURL               string                                     `json:"publicUrl"`
	DirectNodes             map[string]legacyimport.DirectNodeApproval `json:"directNodes"`
	MasterListen            string                                     `json:"masterListen"`
	EffectiveNodeControl    map[string]map[string]string               `json:"effectiveNodeControl"`
	AcceptGoRuntimeDefaults bool                                       `json:"acceptGoRuntimeDefaults"`
	LegacyTimezone          string                                     `json:"legacyTimezone"`
}

func printMigration(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
func migrate(ctx context.Context, args []string, out, errOut io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: oss-transfer-master migrate inspect|prepare|convert-tasks|prepare-recovery|status|finalize")
	}
	flags := flag.NewFlagSet("migrate "+args[0], flag.ContinueOnError)
	flags.SetOutput(errOut)
	switch args[0] {
	case "inspect":
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() == 0 {
			return errors.New("inspect requires one export path per old node")
		}
		plans := make([]legacyimport.Plan, 0, flags.NArg())
		for _, path := range flags.Args() {
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			plan, inspectErr := legacyimport.Inspect(ctx, file)
			closeErr := file.Close()
			if inspectErr != nil {
				return inspectErr
			}
			if closeErr != nil {
				return closeErr
			}
			plans = append(plans, plan)
		}
		plan, err := legacyimport.Combine(plans)
		if err != nil {
			return err
		}
		return printMigration(out, plan)
	case "prepare":
		planPath := flags.String("plan", "", "Private migration plan JSON")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *planPath == "" || flags.NArg() != 0 {
			return errors.New("prepare requires --plan <private JSON file>")
		}
		info, err := os.Lstat(*planPath)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 4<<20 {
			return errors.New("migration plan must be a private regular file (0600)")
		}
		file, err := os.Open(*planPath)
		if err != nil {
			return err
		}
		decoder := json.NewDecoder(io.LimitReader(file, 4<<20))
		decoder.DisallowUnknownFields()
		var plan migrationPlan
		decodeErr := decoder.Decode(&plan)
		var extra any
		trailing := decoder.Decode(&extra)
		file.Close()
		if decodeErr != nil || trailing != io.EOF {
			return errors.New("invalid migration plan JSON")
		}
		prepared, err := legacyimport.Prepare(ctx, legacyimport.PrepareOptions{SnapshotPaths: plan.Snapshots, Destination: plan.Destination, PublicURL: plan.PublicURL, DirectNodes: plan.DirectNodes})
		if err != nil {
			return err
		}
		defer prepared.Close()
		// Report the private staging location before later validation can fail. The
		// original installation and requested destination have not been changed.
		if _, err := fmt.Fprintf(errOut, "Migration staging: %s\n", prepared.Directory); err != nil {
			return err
		}
		if plan.AcceptGoRuntimeDefaults {
			if err := prepared.AcceptRuntimeDefaults(ctx); err != nil {
				return err
			}
		}
		if plan.MasterListen != "" || len(plan.EffectiveNodeControl) > 0 {
			if err := prepared.WriteRuntimeConfig(ctx, plan.MasterListen, plan.EffectiveNodeControl); err != nil {
				return err
			}
		}
		if _, err := prepared.ConvertTasks(ctx, legacyimport.TaskImportOptions{LegacyTimezone: plan.LegacyTimezone}); err != nil {
			return err
		}
		status, err := prepared.Status()
		if err != nil {
			return err
		}
		return printMigration(out, status)
	case "status", "convert-tasks", "prepare-recovery", "finalize":
		directory := flags.String("directory", "", "Existing private staging directory")
		timezone := flags.String("legacy-timezone", "", "Original JVM timezone for saved date filters")
		acceptDefaults := flags.Bool("accept-go-runtime-defaults", false, "Accept the documented Go retry, concurrency and multipart defaults")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *directory == "" || flags.NArg() != 0 {
			return errors.New("migration operation requires --directory <staging directory>")
		}
		prepared, err := legacyimport.OpenPrepared(ctx, *directory)
		if err != nil {
			return err
		}
		defer prepared.Close()
		if args[0] == "finalize" {
			if *acceptDefaults {
				if err := prepared.AcceptRuntimeDefaults(ctx); err != nil {
					return err
				}
			}
			if err := prepared.Finalize(ctx); err != nil {
				return err
			}
			return printMigration(out, map[string]any{"directory": prepared.Directory, "migrationId": prepared.Manifest.MigrationID, "pending": false})
		}
		if args[0] == "convert-tasks" {
			if _, err := prepared.ConvertTasks(ctx, legacyimport.TaskImportOptions{LegacyTimezone: *timezone}); err != nil {
				return err
			}
		}
		if args[0] == "prepare-recovery" {
			if _, err := prepared.PrepareRecovery(ctx); err != nil {
				return err
			}
		}
		status, err := prepared.Status()
		if err != nil {
			return err
		}
		return printMigration(out, status)
	default:
		return errors.New("unknown migration operation; use inspect, prepare, convert-tasks, prepare-recovery, status or finalize")
	}
}
