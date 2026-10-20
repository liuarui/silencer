package gateway

import (
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func configureCaptureProcess(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
}
func signalCaptureProcess(cmd *exec.Cmd, signal os.Signal) error { return cmd.Process.Signal(signal) }
func confirmCaptureChildrenStopped(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	// A process without its job handle cannot prove that its children stopped.
	return errors.New("capture_stop_failed")
}

type captureWindowsJob struct {
	mu     sync.Mutex
	handle windows.Handle
}

func startCaptureProcess(cmd *exec.Cmd) (func() error, error) {
	configureCaptureProcess(cmd)
	handle, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	job := &captureWindowsJob{handle: handle}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err = windows.SetInformationJobObject(handle, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		windows.CloseHandle(handle)
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		windows.CloseHandle(handle)
		return nil, err
	}
	// Start suspended: even an immediately spawning executable cannot escape
	// containment before AssignProcessToJobObject has completed.
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err == nil {
		err = windows.AssignProcessToJobObject(handle, process)
		windows.CloseHandle(process)
	}
	if err == nil {
		err = resumeCapturePrimaryThread(uint32(cmd.Process.Pid))
	}
	if err != nil {
		_ = cmd.Process.Kill()
		_ = windows.TerminateJobObject(handle, 1)
		_ = cmd.Wait()
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	return job.stop, nil
}

func resumeCapturePrimaryThread(pid uint32) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	var primary uint32
	for err = windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID == pid {
			if primary != 0 {
				return errors.New("capture_primary_thread_ambiguous")
			}
			primary = entry.ThreadID
		}
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return err
	}
	if primary == 0 {
		return errors.New("capture_primary_thread_missing")
	}
	thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, primary)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(thread)
	previous, err := windows.ResumeThread(thread)
	if err != nil {
		return err
	}
	if previous != 1 {
		return errors.New("capture_primary_thread_not_resumed")
	}
	return nil
}

func (job *captureWindowsJob) stop() error {
	job.mu.Lock()
	defer job.mu.Unlock()
	if job.handle == 0 {
		return nil
	}
	if err := windows.TerminateJobObject(job.handle, 1); err != nil {
		return errors.New("capture_stop_failed")
	}
	type accounting struct {
		TotalUserTime, TotalKernelTime, PeriodUserTime, PeriodKernelTime int64
		PageFaults, TotalProcesses, ActiveProcesses, TerminatedProcesses uint32
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		info := accounting{}
		if err := windows.QueryInformationJobObject(job.handle, windows.JobObjectBasicAccountingInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil); err != nil {
			return errors.New("capture_stop_failed")
		}
		if info.ActiveProcesses == 0 {
			if err := windows.CloseHandle(job.handle); err != nil {
				return errors.New("capture_stop_failed")
			}
			job.handle = 0
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("capture_stop_failed")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func detachMaintenance(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000008 | 0x01000000}
}
