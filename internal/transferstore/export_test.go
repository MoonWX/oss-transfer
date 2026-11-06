package transferstore

import (
	"context"
	"io"
)

// WriteRangesForTest runs a ranged write into w with the window Write would
// use under budget. External tests use it to pair the ordered writer with the
// real bandwidth limiter, which imports this package.
func (s *Local) WriteRangesForTest(ctx context.Context, w io.Writer, ranges RangeFunc, size int64, budget *RangeBudget) error {
	return s.writeRanges(ctx, w, ranges, size, s.windowSlots(budget))
}
