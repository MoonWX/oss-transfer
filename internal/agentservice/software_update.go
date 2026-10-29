package agentservice

import (
	"context"
	"errors"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/backupretention"
	"github.com/MoonWX/oss-transfer/internal/softwareupdate"
)

func (s *Service) softwareUpdating() bool {
	return s.Updater != nil && softwareupdate.InProgress(s.Updater.Status().Phase)
}

func (s *Service) softwareCommand(ctx context.Context, command agenttransport.Command) (agenttransport.Response, bool, error) {
	if command.Operation != "software_info" && command.Operation != "software_update" && command.Operation != "backup_list" && command.Operation != "backup_prune" {
		return agenttransport.Response{}, false, nil
	}
	finish := func(value any, err error) (agenttransport.Response, bool, error) {
		reply, err := response(value, err)
		return reply, true, err
	}
	if command.Operation == "backup_list" || command.Operation == "backup_prune" {
		var request struct{}
		if err := decode(command.Payload, &request); err != nil {
			return finish(nil, err)
		}
		controller, ok := s.Updater.(softwareupdate.BackupController)
		if command.Operation == "backup_list" {
			if !ok {
				return finish(softwareupdate.BackupSummary{Reason: "UNMANAGED"}, nil)
			}
			value, err := controller.BackupStats(ctx)
			if err != nil {
				value = softwareupdate.BackupSummary{Reason: "ERROR"}
				if errors.Is(err, backupretention.ErrBusy) {
					value.Reason = "BUSY"
				}
			}
			return finish(value, nil)
		}
		if !command.Mutating {
			reply, err := badRequest(errors.New("operation requires a mutating command"))
			return reply, true, err
		}
		if !ok {
			return finish(nil, errors.New("Backup maintenance is unavailable"))
		}
		s.softwareMu.Lock()
		defer s.softwareMu.Unlock()
		value, err := controller.PruneBackups(ctx)
		return finish(value, err)
	}
	if s.Updater == nil {
		return finish(nil, errors.New("Online updates are unavailable for this Agent"))
	}
	if command.Operation == "software_info" {
		var request struct{}
		if err := decode(command.Payload, &request); err != nil {
			return finish(nil, err)
		}
		return finish(map[string]any{"info": s.Updater.Info(), "update": s.Updater.Status()}, nil)
	}
	if !command.Mutating {
		reply, err := badRequest(errors.New("operation requires a mutating command"))
		return reply, true, err
	}
	var request struct {
		Version string `json:"version"`
	}
	if err := decode(command.Payload, &request); err != nil {
		return finish(nil, err)
	}
	s.softwareMu.Lock()
	defer s.softwareMu.Unlock()
	status, err := s.Updater.Start(ctx, request.Version)
	return finish(status, err)
}
