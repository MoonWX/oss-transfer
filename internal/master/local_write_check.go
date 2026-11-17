package master

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/agenttransport"
)

// localPathWriteNotChecked marks a download destination on an Agent from
// before the write probe: its path was checked, but not whether the Agent can
// write there or how much space is left.
const localPathWriteNotChecked = "LOCAL_PATH_WRITE_NOT_CHECKED"

// checkDownloadWritable asks the Agent to create and remove a file in a
// download destination, or the nearest existing directory above it, and
// records where it wrote, as which user and the free space there. Uploads
// pass unchecked. Agents from before the probe pass with the code
// localPathWriteNotChecked; the task then reports any write failure per
// object, as before. Creating a task runs it for every selected node, adding
// nodes for every added node.
func (s *Server) checkDownloadWritable(ctx context.Context, session agenttransport.Session, direction, path string, check *bindingCheck) (string, bool) {
	if direction != "OSS_TO_LOCAL" {
		return "", true
	}
	var probe agentservice.WriteProbe
	if err := s.agentCall(ctx, session, "local_write_probe", false, agentservice.PathRequest{Path: path}, &probe); err != nil {
		if comparisonUnsupported(err) {
			check.Code = localPathWriteNotChecked
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
		switch {
		case probe.EffectiveUID != nil && *probe.EffectiveUID == 0:
			// File permissions never refuse root, so ownership is not the
			// cause and chown cannot help. Under NFS root_squash the client
			// cannot even run chown.
			return fmt.Sprintf(localizeContext(ctx, "节点无权写入 %s：Agent 以 root 运行仍被拒绝，常见原因是 NFS 的 root_squash 将 root 映射为匿名用户（通常为 UID 65534）、FUSE 挂载未启用 allow_other，或 SELinux、macOS 隐私保护等安全策略；请让该目录对映射后的用户可写，或调整导出选项、挂载选项和安全策略"), directory)
		case probe.EffectiveUID != nil:
			uid := strconv.Itoa(*probe.EffectiveUID)
			return fmt.Sprintf(localizeContext(ctx, "节点无权写入 %s：Agent 以 UID %s 运行，请将该目录授权给此用户（例如 chown %s %s）；网络文件系统可能会把此 UID 映射为其他用户"), directory, uid, uid, shellWord(directory))
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

// shellWord is value as one word of a POSIX shell command, quoted only when
// it needs to be, so a suggested command can be pasted as it is.
func shellWord(value string) string {
	if value != "" && strings.IndexFunc(value, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._-+:@%,=", r))
	}) < 0 {
		return value
	}
	return shellQuote(value)
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
