package agentservice

import "time"

// processCPU is not measured on Windows.
func processCPU() time.Duration { return 0 }
