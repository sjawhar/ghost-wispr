//go:build !linux || (!amd64 && !arm64)

package indicator

func OpenBlink1(_, _ string) (Light, error) {
	return nil, ErrNoBlink1
}
