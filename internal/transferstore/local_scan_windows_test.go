//go:build windows

package transferstore

import "errors"

func makeFIFO(string) error { return errors.New("FIFOs are not available on Windows") }
