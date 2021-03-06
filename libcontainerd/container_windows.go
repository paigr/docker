package libcontainerd

import (
	"io"
	"strings"
	"syscall"
	"time"

	"github.com/Microsoft/hcsshim"
	"github.com/Sirupsen/logrus"
)

type container struct {
	containerCommon

	// Platform specific fields are below here. There are none presently on Windows.
	options []CreateOption

	// The ociSpec is required, as client.Create() needs a spec,
	// but can be called from the RestartManager context which does not
	// otherwise have access to the Spec
	ociSpec Spec

	manualStopRequested bool
}

func (ctr *container) newProcess(friendlyName string) *process {
	return &process{
		processCommon: processCommon{
			containerID:  ctr.containerID,
			friendlyName: friendlyName,
			client:       ctr.client,
		},
	}
}

func (ctr *container) start() error {
	var err error

	// Start the container.  If this is a servicing container, this call will block
	// until the container is done with the servicing execution.
	logrus.Debugln("Starting container ", ctr.containerID)
	if err = hcsshim.StartComputeSystem(ctr.containerID); err != nil {
		logrus.Errorf("Failed to start compute system: %s", err)
		return err
	}

	for _, option := range ctr.options {
		if s, ok := option.(*ServicingOption); ok && s.IsServicing {
			// Since the servicing operation is complete when StartCommputeSystem returns without error,
			// we can shutdown (which triggers merge) and exit early.
			const shutdownTimeout = 5 * 60 * 1000  // 4 minutes
			const terminateTimeout = 1 * 60 * 1000 // 1 minute
			if err := hcsshim.ShutdownComputeSystem(ctr.containerID, shutdownTimeout, ""); err != nil {
				logrus.Errorf("Failed during cleanup of servicing container: %s", err)
				// Terminate the container, ignoring errors.
				if err2 := hcsshim.TerminateComputeSystem(ctr.containerID, terminateTimeout, ""); err2 != nil {
					logrus.Errorf("Failed to terminate container %s after shutdown failure: %q", ctr.containerID, err2)
				}
				return err
			}
			return nil
		}
	}

	createProcessParms := hcsshim.CreateProcessParams{
		EmulateConsole:   ctr.ociSpec.Process.Terminal,
		WorkingDirectory: ctr.ociSpec.Process.Cwd,
		ConsoleSize:      ctr.ociSpec.Process.InitialConsoleSize,
	}

	// Configure the environment for the process
	createProcessParms.Environment = setupEnvironmentVariables(ctr.ociSpec.Process.Env)
	createProcessParms.CommandLine = strings.Join(ctr.ociSpec.Process.Args, " ")

	iopipe := &IOPipe{Terminal: ctr.ociSpec.Process.Terminal}

	// Start the command running in the container. Note we always tell HCS to
	// create stdout as it's required regardless of '-i' or '-t' options, so that
	// docker can always grab the output through logs. We also tell HCS to always
	// create stdin, even if it's not used - it will be closed shortly. Stderr
	// is only created if it we're not -t.
	var pid uint32
	var stdout, stderr io.ReadCloser
	pid, iopipe.Stdin, stdout, stderr, err = hcsshim.CreateProcessInComputeSystem(
		ctr.containerID,
		true,
		true,
		!ctr.ociSpec.Process.Terminal,
		createProcessParms)
	if err != nil {
		logrus.Errorf("CreateProcessInComputeSystem() failed %s", err)

		// Explicitly terminate the compute system here.
		if err2 := hcsshim.TerminateComputeSystem(ctr.containerID, hcsshim.TimeoutInfinite, "CreateProcessInComputeSystem failed"); err2 != nil {
			// Ignore this error, there's not a lot we can do except log it
			logrus.Warnf("Failed to TerminateComputeSystem after a failed CreateProcessInComputeSystem. Ignoring this.", err2)
		} else {
			logrus.Debugln("Cleaned up after failed CreateProcessInComputeSystem by calling TerminateComputeSystem")
		}
		return err
	}
	ctr.startedAt = time.Now()

	// Convert io.ReadClosers to io.Readers
	if stdout != nil {
		iopipe.Stdout = openReaderFromPipe(stdout)
	}
	if stderr != nil {
		iopipe.Stderr = openReaderFromPipe(stderr)
	}

	// Save the PID
	logrus.Debugf("Process started - PID %d", pid)
	ctr.systemPid = uint32(pid)

	// Spin up a go routine waiting for exit to handle cleanup
	go ctr.waitExit(pid, InitFriendlyName, true)

	ctr.client.appendContainer(ctr)

	if err := ctr.client.backend.AttachStreams(ctr.containerID, *iopipe); err != nil {
		// OK to return the error here, as waitExit will handle tear-down in HCS
		return err
	}

	// Tell the docker engine that the container has started.
	si := StateInfo{
		CommonStateInfo: CommonStateInfo{
			State: StateStart,
			Pid:   ctr.systemPid, // Not sure this is needed? Double-check monitor.go in daemon BUGBUG @jhowardmsft
		}}
	return ctr.client.backend.StateChanged(ctr.containerID, si)

}

// waitExit runs as a goroutine waiting for the process to exit. It's
// equivalent to (in the linux containerd world) where events come in for
// state change notifications from containerd.
func (ctr *container) waitExit(pid uint32, processFriendlyName string, isFirstProcessToStart bool) error {
	logrus.Debugln("waitExit on pid", pid)

	// Block indefinitely for the process to exit.
	exitCode, err := hcsshim.WaitForProcessInComputeSystem(ctr.containerID, pid, hcsshim.TimeoutInfinite)
	if err != nil {
		if herr, ok := err.(*hcsshim.HcsError); ok && herr.Err != syscall.ERROR_BROKEN_PIPE {
			logrus.Warnf("WaitForProcessInComputeSystem failed (container may have been killed): %s", err)
		}
		// Fall through here, do not return. This ensures we attempt to continue the
		// shutdown in HCS nad tell the docker engine that the process/container
		// has exited to avoid a container being dropped on the floor.
	}

	// Assume the container has exited
	si := StateInfo{
		CommonStateInfo: CommonStateInfo{
			State:     StateExit,
			ExitCode:  uint32(exitCode),
			Pid:       pid,
			ProcessID: processFriendlyName,
		},
		UpdatePending: false,
	}

	// But it could have been an exec'd process which exited
	if !isFirstProcessToStart {
		si.State = StateExitProcess
	} else {
		// Since this is the init process, always call into vmcompute.dll to
		// shutdown the container after we have completed.

		propertyCheckFlag := 1 // Include update pending check.
		csProperties, err := hcsshim.GetComputeSystemProperties(ctr.containerID, uint32(propertyCheckFlag))
		if err != nil {
			logrus.Warnf("GetComputeSystemProperties failed (container may have been killed): %s", err)
		} else {
			si.UpdatePending = csProperties.AreUpdatesPending
		}

		logrus.Debugf("Shutting down container %s", ctr.containerID)
		// Explicit timeout here rather than hcsshim.TimeoutInfinte to avoid a
		// (remote) possibility that ShutdownComputeSystem hangs indefinitely.
		const shutdownTimeout = 5 * 60 * 1000 // 5 minutes
		if err := hcsshim.ShutdownComputeSystem(ctr.containerID, shutdownTimeout, "waitExit"); err != nil {
			if herr, ok := err.(*hcsshim.HcsError); !ok ||
				(herr.Err != hcsshim.ERROR_SHUTDOWN_IN_PROGRESS &&
					herr.Err != ErrorBadPathname &&
					herr.Err != syscall.ERROR_PATH_NOT_FOUND) {
				logrus.Debugf("waitExit - error from ShutdownComputeSystem on %s %v. Calling TerminateComputeSystem", ctr.containerCommon, err)
				if err := hcsshim.TerminateComputeSystem(ctr.containerID, shutdownTimeout, "waitExit"); err != nil {
					logrus.Debugf("waitExit - ignoring error from TerminateComputeSystem %s %v", ctr.containerID, err)
				} else {
					logrus.Debugf("Successful TerminateComputeSystem after failed ShutdownComputeSystem on %s in waitExit", ctr.containerID)
				}
			}
		} else {
			logrus.Debugf("Completed shutting down container %s", ctr.containerID)
		}

		if !ctr.manualStopRequested && ctr.restartManager != nil {
			restart, wait, err := ctr.restartManager.ShouldRestart(uint32(exitCode), false, time.Since(ctr.startedAt))
			if err != nil {
				logrus.Error(err)
			} else if restart {
				si.State = StateRestart
				ctr.restarting = true
				go func() {
					err := <-wait
					ctr.restarting = false
					ctr.client.deleteContainer(ctr.friendlyName)
					if err != nil {
						si.State = StateExit
						if err := ctr.client.backend.StateChanged(ctr.containerID, si); err != nil {
							logrus.Error(err)
						}
						logrus.Error(err)
					} else {
						ctr.client.Create(ctr.containerID, ctr.ociSpec, ctr.options...)
					}
				}()
			}
		}

		// Remove process from list if we have exited
		// We need to do so here in case the Message Handler decides to restart it.
		if si.State == StateExit {
			ctr.client.deleteContainer(ctr.friendlyName)
		}
	}

	// Call into the backend to notify it of the state change.
	logrus.Debugf("waitExit() calling backend.StateChanged %v", si)
	if err := ctr.client.backend.StateChanged(ctr.containerID, si); err != nil {
		logrus.Error(err)
	}

	logrus.Debugln("waitExit() completed OK")
	return nil
}
