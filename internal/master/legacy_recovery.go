package master

import (
	"context"
	"errors"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/task"
)

func (s *Server) resolveLegacySource(ctx context.Context, p task.Parent) (task.Spec, error) {
	spec := p.Spec
	if spec.Source.Kind != "LOCAL" {
		return spec, nil
	}
	spec.Nodes = append([]task.Node(nil), spec.Nodes...)
	kind := ""
	for i, node := range spec.Nodes {
		var probe struct {
			PathType string `json:"pathType"`
		}
		if err := s.agentCall(ctx, nodeSession(node), "local_path", false, agentservice.PathRequest{Path: node.LocalRoot}, &probe); err != nil {
			return spec, err
		}
		if probe.PathType != "FILE" && probe.PathType != "DIRECTORY" {
			return spec, errors.New("legacy local source is missing")
		}
		if kind != "" && kind != probe.PathType {
			return spec, errors.New("legacy local source type differs between nodes")
		}
		kind = probe.PathType
		if kind == "FILE" {
			root, key := splitAgentFile(node.LocalRoot)
			spec.Nodes[i].LocalRoot = root
			if i == 0 {
				spec.Source.Root = root
				spec.ObjectKey = key
			} else if key != spec.ObjectKey {
				return spec, errors.New("legacy source filename differs between nodes")
			}
		}
	}
	if len(spec.Nodes) > 1 {
		spec.SharedLocalSource = true
	}
	return spec, nil
}
