package cli

import (
	"os"

	"github.com/steading-ai/skift/internal/admin"
)

// SetDoctorExecutable makes doctor's mcp check start the binary at path,
// or fail with err, and returns how to undo it. In a spec this process is
// the test binary, which is not skift.
func SetDoctorExecutable(path string, err error) (restore func()) {
	doctorExecutable = func() (string, error) { return path, err }
	return func() { doctorExecutable = os.Executable }
}

// RunJobsUnderServe adds jobs to what every serve started from now on runs,
// and returns the function that takes them away again.
func RunJobsUnderServe(jobs ...admin.Job) (restore func()) {
	previous := extraJobs
	extraJobs = append(append([]admin.Job(nil), previous...), jobs...)
	return func() { extraJobs = previous }
}
