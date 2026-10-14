package master

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/MoonWX/oss-transfer/internal/task"
)

func legacyChildDTO(p task.Parent, child task.LegacyChild) (map[string]any, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(child.Raw, &raw); err != nil {
		return nil, err
	}
	row := taskDTO(p)
	for _, name := range []string{"direction", "status", "sourceS3Alias", "sourceBucket", "destS3Alias", "destBucket", "objectKey", "targetRelativePath", "localPath", "startKey", "endKey", "marker", "scanComplete", "conflictPolicy", "overwrite", "distributionMode", "dateAfter", "dateBefore", "listObjectCount", "listObjectSize", "putObjectCount", "putObjectSize", "skippedObjectCount", "skippedObjectSize", "failedObjectCount", "failedObjectSize", "createTime", "startTime", "endTime", "errorCode", "errorMessage", "attempt"} {
		if value, ok := raw[name]; ok {
			var out any
			decoder := json.NewDecoder(bytes.NewReader(value))
			decoder.UseNumber()
			if err := decoder.Decode(&out); err != nil {
				return nil, err
			}
			row[name] = out
		} else {
			delete(row, name)
		}
	}
	for _, name := range []string{"sourceEndpoint", "destinationEndpoint"} {
		if value, ok := raw[name]; ok {
			var endpoint taskEndpoint
			if err := json.Unmarshal(value, &endpoint); err != nil {
				return nil, err
			}
			row[name] = endpoint
		}
	}
	if value, ok := raw["keyMapping"]; ok {
		var mapping taskMapping
		if err := json.Unmarshal(value, &mapping); err != nil {
			return nil, err
		}
		row["keyMapping"] = mapping
	}
	original := row["status"]
	if original != "COMPLETED" && original != "FAILED" && original != "PARTIAL_FAILED" {
		row["status"] = "STOPPED"
	}
	row["legacyOriginalStatus"] = original
	row["legacyHistorical"] = true
	row["contentVerified"] = false
	row["taskIndex"] = p.ID
	row["parentTaskId"] = p.ID
	row["childTaskId"] = fmt.Sprintf("%d-%d", p.ID, child.Number)
	row["shardId"] = row["childTaskId"]
	row["shardIndex"] = child.Number - 1
	row["subtaskNumber"] = child.Number
	row["workerNode"] = child.Node
	row["executionNodeIp"] = child.Node
	row["ossTaskGroup"] = false
	row["autoStart"] = false
	delete(row, "creationStatus")
	delete(row, "childCount")
	delete(row, "averageSpeedBytesPerSecond")
	delete(row, "currentSpeedBytesPerSecond")
	return row, nil
}
func (s *Server) walkLegacyChildren(ctx context.Context, p task.Parent, visit func(task.LegacyChild) error) error {
	after := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		rows, err := s.tasks.repo.LegacyChildren(p.ID, after, 128)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if err := visit(row); err != nil {
				return err
			}
			after = row.Number
		}
		if len(rows) < 128 {
			return nil
		}
	}
}
func legacySummaryCSV(writer *csv.Writer, p task.Parent, child task.LegacyChild) error {
	row, err := legacyChildDTO(p, child)
	if err != nil {
		return err
	}
	values := []any{"CHILD_TASK", p.ID, row["childTaskId"], row["direction"], child.Node, row["legacyOriginalStatus"], reportEndpoint(p.Spec.Source), reportEndpoint(p.Spec.Destination), p.Spec.Prefix, row["startKey"], row["endKey"], row["objectKey"], row["targetRelativePath"], row["dateAfter"], row["dateBefore"], row["overwrite"], row["conflictPolicy"], row["distributionMode"], row["listObjectCount"], row["listObjectSize"], row["putObjectCount"], row["putObjectSize"], row["skippedObjectCount"], row["skippedObjectSize"], row["failedObjectCount"], row["failedObjectSize"], row["createTime"], row["startTime"], row["endTime"], p.Spec.MappingMode, p.Spec.ReplaceSourcePrefix, p.Spec.TargetPrefix, p.Spec.TargetKey}
	return reportCSV(writer, append(values, reportRequestLimits(p.Spec.RequestLimits)...)...)
}
func (s *Server) walkLegacyHistory(ctx context.Context, p task.Parent, kind string, visit func(task.LegacyHistory) error) error {
	after := ""
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		rows, err := s.tasks.repo.LegacyHistory(p.ID, kind, after, 128)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if err := visit(row); err != nil {
				return err
			}
			after = row.Cursor
		}
		if len(rows) < 128 {
			return nil
		}
	}
}
func legacyTimestamp(value *int64) any {
	if value == nil {
		return nil
	}
	return time.UnixMilli(*value).UTC()
}
func (s *Server) writeLegacyReport(ctx context.Context, p task.Parent, files, issues *csv.Writer) (reportCoverage, error) {
	coverage := newCoverage(p)
	coverage.LegacyHistorical = true
	coverage.LegacyOriginalStatus = p.Legacy.OriginalStatus
	var ledgerCount int64
	allTracked := true
	var discovered int64
	err := s.walkLegacyHistory(ctx, p, "COVERAGE", func(row task.LegacyHistory) error {
		if !strings.HasSuffix(row.Key, ":coverage") {
			return nil
		}
		var saved struct {
			LedgerCoverage  string
			ScanComplete    bool
			DiscoveredCount int64
		}
		if err := json.Unmarshal(row.Raw, &saved); err != nil {
			return err
		}
		ledgerCount++
		allTracked = allTracked && saved.LedgerCoverage == "TRACKED" && saved.ScanComplete
		discovered += saved.DiscoveredCount
		return nil
	})
	if err != nil {
		return coverage, err
	}
	err = s.walkLegacyHistory(ctx, p, "OBJECT", func(row task.LegacyHistory) error {
		var saved struct {
			Direction, LogicalKey, SourceS3Alias, SourceBucket, SourceKey, SourcePath, DestinationS3Alias, DestinationBucket, DestinationKey, DestinationPath, Node, Result, ReasonCode, ErrorMessage string
			Size                                                                                                                                                                                      *int64
			DiscoveredAt, CompletedAt                                                                                                                                                                 *int64
		}
		if err := json.Unmarshal(row.Raw, &saved); err != nil {
			return err
		}
		status := saved.Result
		switch status {
		case "SUCCESS", "SKIPPED", "FAILED", "UNPROCESSED":
		default:
			status = "UNKNOWN"
		}
		if saved.Node == "" {
			saved.Node = row.Node
		}
		if saved.Direction == "" {
			saved.Direction = p.Spec.Direction
		}
		var size any
		if saved.Size != nil {
			size = *saved.Size
			if *saved.Size < 0 {
				status = "UNKNOWN"
			} else {
				coverage.ResultBytes[status] += *saved.Size
			}
		} else {
			allTracked = false
		}
		if err := reportCSV(files, p.ID, row.ChildTaskID, saved.Direction, saved.LogicalKey, saved.SourceS3Alias, saved.SourceBucket, saved.SourceKey, saved.SourcePath, saved.DestinationS3Alias, saved.DestinationBucket, saved.DestinationKey, saved.DestinationPath, size, saved.Node, status, saved.ReasonCode, saved.ErrorMessage, legacyTimestamp(saved.DiscoveredAt), legacyTimestamp(saved.CompletedAt), nil, nil); err != nil {
			return err
		}
		if status == "FAILED" || status == "UNKNOWN" {
			if err := reportCSV(issues, p.ID, row.ChildTaskID, saved.Direction, saved.LogicalKey, size, saved.Node, saved.ReasonCode, saved.ErrorMessage, legacyTimestamp(saved.CompletedAt)); err != nil {
				return err
			}
		}
		coverage.FileRows++
		coverage.ResultCounts[status]++
		return nil
	})
	if err != nil {
		return coverage, err
	}
	var totalBytes int64
	for _, n := range coverage.ResultBytes {
		totalBytes += n
	}
	coverage.CountsReconciled = coverage.FileRows == p.ScannedObjects && totalBytes == p.ScannedBytes && discovered == coverage.FileRows && coverage.ResultCounts["SUCCESS"] == p.Counters.Succeeded && coverage.ResultCounts["SKIPPED"] == p.Counters.Skipped && coverage.ResultCounts["FAILED"] == p.Counters.Failed && coverage.ResultBytes["SUCCESS"] == p.Counters.SucceededBytes && coverage.ResultBytes["SKIPPED"] == p.Counters.SkippedBytes && coverage.ResultBytes["FAILED"] == p.Counters.FailedBytes && coverage.ResultCounts["UNKNOWN"] == 0
	if ledgerCount == 0 || !allTracked {
		coverage.PartialReasons = append(coverage.PartialReasons, "LEGACY_LEDGER_INCOMPLETE")
	}
	if !p.ScanComplete {
		coverage.PartialReasons = append(coverage.PartialReasons, "SCAN_INCOMPLETE")
	}
	if p.Legacy.NeedsReconciliation {
		coverage.PartialReasons = append(coverage.PartialReasons, "LEGACY_RECOVERY_PENDING")
	}
	if coverage.ResultCounts["UNPROCESSED"] > 0 {
		coverage.PartialReasons = append(coverage.PartialReasons, "UNPROCESSED_OBJECTS")
	}
	if !coverage.CountsReconciled {
		coverage.PartialReasons = append(coverage.PartialReasons, "COUNTS_DO_NOT_RECONCILE")
	}
	current, err := s.tasks.repo.Get(p.ID)
	if err != nil {
		return coverage, err
	}
	coverage.Stable = current.Status == p.Status && current.Generation == p.Generation && current.Purged == p.Purged && current.Deleted == p.Deleted && current.Legacy != nil && current.Legacy.ImportHash == p.Legacy.ImportHash
	if !coverage.Stable {
		coverage.PartialReasons = append(coverage.PartialReasons, "STATE_CHANGED_DURING_EXPORT")
	}
	coverage.Complete = len(coverage.PartialReasons) == 0
	return coverage, nil
}
