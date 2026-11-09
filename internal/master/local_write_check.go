package master

import (
	"context"
	"fmt"
	"strconv"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/cluster"
)

// checkDownloadWritable asks the Agent to create and remove a file in a
// download destination, or the nearest existing directory above it, and
// records where it wrote, as which user and the free space there. Uploads and
// Agents from before the probe pass unchecked; the task then reports any
// write failure per object, as before.
func (s *Server) checkDownloadWritable(ctx context.Context, member cluster.Member, direction, path string, check *bindingCheck) (string, bool) {
	if direction != "OSS_TO_LOCAL" {
		return "", true
	}
	var probe agentservice.WriteProbe
	if err := s.agentCall(ctx, member.Session(), "local_write_probe", false, agentservice.PathRequest{Path: path}, &probe); err != nil {
		if comparisonUnsupported(err) {
			return "", true
		}
		return err.Error(), false
	}
	check.WriteProbeDirectory, check.EffectiveUID, check.FreeBytes = probe.ProbeDirectory, probe.EffectiveUID, probe.FreeBytes
	if probe.Writable {
		return "", true
	}
	return writeProbeMessage(ctx, probe), false
}

func writeProbeMessage(ctx context.Context, probe agentservice.WriteProbe) string {
	directory := probe.ProbeDirectory
	if directory == "" {
		directory = probe.Path
	}
	switch probe.ErrorCode {
	case agentservice.WriteProbePermissionDenied:
		if probe.EffectiveUID != nil {
			return fmt.Sprintf(localizeContext(ctx, "节点无权写入 %s：Agent 以 UID %s 运行，请将该目录授权给此用户（例如 chown %s）"), directory, strconv.Itoa(*probe.EffectiveUID), strconv.Itoa(*probe.EffectiveUID))
		}
		return fmt.Sprintf(localizeContext(ctx, "节点无权写入 %s，请为 Agent 服务账户授予写权限"), directory)
	case agentservice.WriteProbeReadOnly:
		return fmt.Sprintf(localizeContext(ctx, "%s 所在文件系统为只读，无法下载到此目录"), directory)
	case agentservice.WriteProbeNoSpace:
		return fmt.Sprintf(localizeContext(ctx, "%s 所在磁盘已无剩余空间"), directory)
	case agentservice.WriteProbeNotDirectory:
		return fmt.Sprintf(localizeContext(ctx, "%s 不是目录，无法作为下载目标"), directory)
	}
	return fmt.Sprintf(localizeContext(ctx, "无法在 %s 中写入测试文件：%s"), directory, probe.Error)
}

// formatBinaryBytes renders a byte count with a binary unit, such as 12.3 GiB.
func formatBinaryBytes(value int64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}
	size, unit := float64(value), 0
	for size >= 1024 && unit < len(units)-1 {
		size /= 1024
		unit++
	}
	if unit == 0 {
		return strconv.FormatInt(value, 10) + " B"
	}
	return strconv.FormatFloat(size, 'f', 1, 64) + " " + units[unit]
}
