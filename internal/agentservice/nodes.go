package agentservice

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/nodecontrol"
	"github.com/MoonWX/oss-transfer/internal/storageconfig"
)

// configureNodeFactory installs a transport below the S3 SDK so retries and
// multipart requests all spend the same Agent-wide request budget. The SDK
// remains responsible for V2/V4 signing.
func configureNodeFactory(factory *storageconfig.Factory, nodes *nodecontrol.Controller) {
	factory.HTTPClientForEndpoint = func(ctx context.Context, endpoint execution.Endpoint, config storageconfig.Config) *http.Client {
		info := nodecontrol.RequestContextFrom(ctx)
		info.ConnectionAlias, info.Bucket = endpoint.Ref, endpoint.Bucket
		info.PathStyle = config.PathStyleEnable != nil && *config.PathStyleEnable
		var client http.Client
		if factory.HTTPClient != nil {
			client = *factory.HTTPClient
		}
		client.Transport = nodes.Transport(client.Transport, info)
		return &client
	}
}

func nodeResponse(value any, err error) (agenttransport.Response, error) {
	if errors.Is(err, nodecontrol.ErrInvalid) {
		return badRequest(err)
	}
	if errors.Is(err, nodecontrol.ErrNotFound) {
		data, _ := json.Marshal(map[string]string{"code": "not_found", "message": err.Error()})
		return agenttransport.Response{Status: 404, Payload: data}, nil
	}
	return response(value, err)
}

func (s *Service) nodeCommand(ctx context.Context, command agenttransport.Command) (agenttransport.Response, bool, error) {
	switch command.Operation {
	case "node_status", "node_details", "node_config", "node_config_set", "node_rate_set", "node_rate_history", "node_rate_detail":
	default:
		return agenttransport.Response{}, false, nil
	}
	respond := func(value any, err error) (agenttransport.Response, bool, error) {
		out, e := nodeResponse(value, err)
		return out, true, e
	}
	if s.Nodes == nil {
		return respond(nil, errors.New("node controller is unavailable"))
	}
	if (command.Operation == "node_config_set" || command.Operation == "node_rate_set") && !command.Mutating {
		return respond(nil, nodecontrol.ErrInvalid)
	}
	switch command.Operation {
	case "node_details":
		details := hostDetails()
		details["advertisedLocalRoots"] = s.Nodes.Config().AdvertisedLocalRoots
		return respond(details, nil)
	case "node_status":
		var request struct{}
		if err := decode(command.Payload, &request); err != nil {
			return respond(nil, nodecontrol.ErrInvalid)
		}
		rates, err := s.Nodes.Snapshot()
		if err != nil {
			return respond(nil, err)
		}
		history, err := s.Nodes.History(50)
		if err != nil {
			return respond(nil, err)
		}
		unfinished, maxUnfinished, err := s.Jobs.Unfinished()
		if err != nil {
			return respond(nil, err)
		}
		children, maxChildren := s.Engine.Concurrency()
		scans, maxScans := s.Scanner.Concurrency()
		execution := map[string]any{"activeScans": scans, "maxConcurrentScans": maxScans, "activeChildren": children, "maxConcurrentChildren": maxChildren, "unfinishedChildren": unfinished, "maxUnfinishedChildren": maxUnfinished}
		return respond(map[string]any{"success": true, "rateLimit": rates, "rateLimitHistory": history, "localStorage": s.Nodes.Config(), "executionStatus": execution}, nil)
	case "node_config":
		var request struct{}
		if err := decode(command.Payload, &request); err != nil {
			return respond(nil, nodecontrol.ErrInvalid)
		}
		return respond(s.Nodes.Config(), nil)
	case "node_config_set":
		var request nodecontrol.Config
		if err := decode(command.Payload, &request); err != nil {
			return respond(nil, nodecontrol.ErrInvalid)
		}
		if err := s.Nodes.SetRoots(ctx, request.AdvertisedLocalRoots); err != nil {
			return respond(nil, err)
		}
		return respond(s.Nodes.Config(), nil)
	case "node_rate_set":
		var request struct {
			Type string   `json:"type"`
			QPS  *float64 `json:"qps"`
		}
		if err := decode(command.Payload, &request); err != nil || request.QPS == nil {
			return respond(nil, nodecontrol.ErrInvalid)
		}
		if err := s.Nodes.SetRate(ctx, request.Type, *request.QPS); err != nil {
			return respond(nil, err)
		}
		rates, err := s.Nodes.Snapshot()
		return respond(rates, err)
	case "node_rate_history":
		var request struct {
			Limit int `json:"limit"`
		}
		if err := decode(command.Payload, &request); err != nil {
			return respond(nil, nodecontrol.ErrInvalid)
		}
		history, err := s.Nodes.History(request.Limit)
		return respond(history, err)
	case "node_rate_detail":
		var request struct {
			EventID string `json:"eventId"`
			Cursor  string `json:"cursor"`
		}
		if err := decode(command.Payload, &request); err != nil {
			return respond(nil, nodecontrol.ErrInvalid)
		}
		detail, err := s.Nodes.Detail(request.EventID, request.Cursor)
		return respond(detail, err)
	}
	panic("unreachable node operation")
}
