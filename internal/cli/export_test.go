package cli

import "os"

// SetDoctorExecutable makes doctor's mcp check start the binary at path,
// or fail with err, and returns how to undo it. In a spec this process is
// the test binary, which is not skift.
func SetDoctorExecutable(path string, err error) (restore func()) {
	doctorExecutable = func() (string, error) { return path, err }
	return func() { doctorExecutable = os.Executable }
}
