/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright 2020 Red Hat, Inc.
 *
 */

package network

import (
	"fmt"
	"os/exec"
	"runtime"
	"syscall"

	"github.com/opencontainers/selinux/go-selinux"

	kvselinux "kubevirt.io/kubevirt/pkg/virt-handler/selinux"
)

type SELinuxContextExecution struct {
	launcherPID  int
	cmdToExecute *exec.Cmd
}

func (ce *SELinuxContextExecution) Execute() error {
	if isSELinuxEnabled() {
		defer func() {
			_ = resetVirtHandlerSELinuxContext()
		}()

		if err := setVirtLauncherSELinuxContext(ce.launcherPID); err != nil {
			return err
		}
	}
	preventFDLeakOntoChild()
	if err := ce.cmdToExecute.Run(); err != nil {
		return fmt.Errorf("failed to execute command in launcher namespace %d: %v", ce.launcherPID, err)
	}
	return nil
}

func isSELinuxEnabled() bool {
	_, selinuxEnabled, err := kvselinux.NewSELinux()
	return err == nil && selinuxEnabled
}

func setVirtLauncherSELinuxContext(virtLauncherPID int) error {
	virtLauncherSELinuxLabel, err := getProcessCurrentSELinuxLabel(virtLauncherPID)
	if err != nil {
		return fmt.Errorf("error reading virt-launcher %d selinux label. Reason: %v", virtLauncherPID, err)
	}

	runtime.LockOSThread()
	if err := selinux.SetExecLabel(virtLauncherSELinuxLabel); err != nil {
		return fmt.Errorf("failed to switch selinux context to %s. Reason: %v", virtLauncherSELinuxLabel, err)
	}
	return nil
}

func resetVirtHandlerSELinuxContext() error {
	virtHandlerSELinuxLabel := "system_u:system_r:spc_t:s0"
	err := selinux.SetExecLabel(virtHandlerSELinuxLabel)
	runtime.UnlockOSThread()
	return err
}

func getProcessCurrentSELinuxLabel(pid int) (string, error) {
	launcherSELinuxLabel, err := selinux.FileLabel(fmt.Sprintf("/proc/%d/attr/current", pid))
	if err != nil {
		return "", fmt.Errorf("could not retrieve pid %d selinux label: %v", pid, err)
	}
	return launcherSELinuxLabel, nil
}

func preventFDLeakOntoChild() {
	// we want to share the parent process std{in|out|err}
	for fd := 3; fd < 256; fd++ {
		syscall.CloseOnExec(fd)
	}
}
