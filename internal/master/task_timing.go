package master

import (
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
)

func executionTimeDTO(row map[string]any, timing execution.ActiveTime, started, finished *time.Time, succeededBytes int64, now time.Time) {
	row["activeDurationMillis"] = timing.ElapsedMillis
	row["durationEstimated"] = timing.Estimated
	row["durationIncomplete"] = timing.Incomplete
	row["activeStartedAt"] = nil
	if timing.StartedAt != nil {
		row["activeStartedAt"] = timing.StartedAt.UnixMilli()
	}
	if started != nil {
		row["startTime"] = started.UnixMilli()
		duration := timing.Duration(now)
		row["duration"] = duration.Round(time.Second).String()
		if timing.Incomplete {
			row["duration"] = "≥ " + row["duration"].(string)
		}
		if duration > 0 && !timing.Incomplete {
			row["averageSpeedBytesPerSecond"] = float64(succeededBytes) / duration.Seconds()
		}
	}
	if finished != nil {
		row["endTime"] = finished.UnixMilli()
	}
}
