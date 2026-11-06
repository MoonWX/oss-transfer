package nodecontrol

import "time"

// throttleKinds are the request limits a throttling response can lower, in
// kindIndex order. Object GETs, multipart listings and aborts have no node
// limit; their responses are counted under throttleOther and lower nothing.
var throttleKinds = [...]string{"list", "put", "head"}

const (
	throttleOther = len(throttleKinds)
	// One-second buckets, more than the longest window (60 s).
	throttleBuckets = 64
)

type throttleSample struct {
	second               int64
	responses, throttled [throttleOther + 1]int64
}

// throttleEvidence is what a reduction was based on.
type throttleEvidence struct {
	count, responses int64
	measured         float64
}

// operationLimit names the node request limit that admits an operation.
func operationLimit(operation string) string {
	switch operation {
	case "ListObjects", "ListBuckets":
		return "list"
	case "PutObject", "UploadPart", "CreateMultipartUpload", "CompleteMultipartUpload":
		return "put"
	case "HeadObject":
		return "head"
	}
	return ""
}

func throttleIndex(operation string) int {
	if index := kindIndex(operationLimit(operation)); index >= 0 && index < throttleOther {
		return index
	}
	return throttleOther
}

func (c *Controller) throttleBucket(second int64) *throttleSample {
	entry := &c.throttles[second%throttleBuckets]
	if entry.second != second {
		*entry = throttleSample{second: second}
	}
	return entry
}

// throttleWindow counts one limit's throttling and total responses in the
// window: the current second and the ones before it, ThrottleWindow seconds in
// all. A reduction blocks the next for ThrottleWindow, after which the window
// no longer holds the second of the reduction, so new evidence is needed.
func (c *Controller) throttleWindow(now time.Time, index int) (throttled, responses int64) {
	last := now.Unix()
	for second := last - int64(c.options.ThrottleWindow/time.Second) + 1; second <= last; second++ {
		if entry := c.throttles[second%throttleBuckets]; entry.second == second {
			throttled += entry.throttled[index]
			responses += entry.responses[index]
		}
	}
	return throttled, responses
}

// peakRate is the most attempts of one limit admitted in any of the last five
// completed seconds, or zero. Lowering from the measured rate takes effect at
// once even when the limit is far above what the node actually sends.
func (c *Controller) peakRate(now time.Time, index int) float64 {
	var peak int64
	for _, entry := range c.samples {
		if entry.second >= now.Unix()-5 && entry.second < now.Unix() {
			peak = max(peak, entry.values[index])
		}
	}
	return float64(peak)
}
