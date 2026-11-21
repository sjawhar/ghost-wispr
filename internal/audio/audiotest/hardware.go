//go:build hardware

package audiotest

// hardware: this test binary was built with the hardware tag, so a test that
// calls UseRealDevices runs and may open real audio devices.
const hardware = true
