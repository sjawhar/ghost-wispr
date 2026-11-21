//go:build !hardware

package audiotest

// hardware: this test binary was built without the hardware tag, so a test
// that calls UseRealDevices is skipped.
const hardware = false
