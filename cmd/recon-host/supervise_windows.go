//go:build windows

package main

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// childJob is a job object that ends the agent, and the encoders it started,
// when the supervisor's process ends: Stop-ScheduledTask ends only the process
// the task started, the supervisor. The handle is never closed; it closes when
// the supervisor's process ends, which ends every process in the job.
type childJob windows.Handle

func newChildJob() (childJob, error) {
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(h, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(h)
		return 0, err
	}
	return childJob(h), nil
}

// add puts a started child into the job; the processes it starts are in it too.
func (j childJob) add(p *os.Process) error {
	if j == 0 {
		return nil
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(p.Pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return windows.AssignProcessToJobObject(windows.Handle(j), h)
}

// stopChild leaves the child to stop by itself: Windows has no signal for one
// process, and a Ctrl+C in a console reaches the child too. The supervisor
// kills it after stopWait.
func stopChild(*os.Process) error { return nil }
