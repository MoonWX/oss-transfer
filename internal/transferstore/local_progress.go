package transferstore

import "os"

// ReportsProgress: Write counts the bytes it has written to the temporary
// file into WriteOptions.Progress as accepted by the destination. Chunks a
// ranged write fetched but has not yet written in order are not counted.
func (s *Local) ReportsProgress() bool { return true }

var _ ProgressReporter = (*Local)(nil)

// progressFile counts every write to a temporary file as confirmed: the
// bytes are in the file Write publishes once it holds all of them. The
// bytes count as sent too, for the speed.
type progressFile struct {
	file     *os.File
	progress WriteProgress
}

func (f *progressFile) Write(p []byte) (int, error) {
	n, err := f.file.Write(p)
	if n > 0 {
		f.progress.Accepted(0, int64(n))
		f.progress.Sent(int64(n))
	}
	return n, err
}
