package agentservice

import (
	"context"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/inventory"
)

func (s *Service) inventoryCommand(ctx context.Context, c agenttransport.Command) (agenttransport.Response, error) {
	switch c.Operation {
	case "inventory_begin":
		var d inventory.Descriptor
		if err := decode(c.Payload, &d); err != nil {
			return badRequest(err)
		}
		if d.ParentID <= 0 {
			return badRequest(inventory.ErrInvalid)
		}
		h, err := s.Inventory.Begin(ctx, d)
		return response(h, err)
	case "inventory_batch":
		var b inventory.Batch
		if err := decode(c.Payload, &b); err != nil {
			return badRequest(err)
		}
		h, err := s.Inventory.Put(ctx, b)
		return response(h, err)
	default:
		var request inventory.Seal
		if err := decode(c.Payload, &request); err != nil {
			return badRequest(err)
		}
		h, err := s.Inventory.Seal(ctx, request)
		return response(h, err)
	}
}
