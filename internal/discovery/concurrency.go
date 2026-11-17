package discovery

import "github.com/MoonWX/oss-transfer/internal/nodecontrol"

// SetConcurrency changes how many scans may run at once. It never cancels a
// running scan: after the value is lowered, Start and FinalizeTakeover refuse
// with ErrCapacity until fewer scans than the new value run. A raised value
// admits new scans at once.
func (s *Scanner) SetConcurrency(scans int) error {
	if err := nodecontrol.ValidateScanConcurrency(scans); err != nil {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.options.MaxConcurrentScans = scans
	return nil
}
