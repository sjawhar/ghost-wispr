//go:build !linux || (!amd64 && !arm64)

package indicator

func OpenBlink1(_, _ string) (Light, error) {
	return nil, ErrNoBlink1
}

func OpenBlink1ForCommand(_, _ string) (Light, error) {
	return nil, ErrNoBlink1
}

// RecordSavesIn does nothing where the blink(1) driver is not built.
func RecordSavesIn(_ string) {}
