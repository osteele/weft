package vastai

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// JobResult contains the outcome of running a job on a Vast.ai instance.
type JobResult struct {
	ExitCode   int
	Stdout     string
	Stderr     string
	ActualCost float64       // estimated cost based on runtime * $/hr
	Runtime    time.Duration // wall-clock time for the job command
}

// ProgressFunc is called at each lifecycle phase to report status.
type ProgressFunc func(phase string)

// RunJobOnInstance handles the full lifecycle of running a job on Vast.ai:
// create instance, wait for ready, sync files, run command, collect output, destroy.
func RunJobOnInstance(client VastaiClient, offer Offer, opts CreateOpts, workDir string, command string, inputs []string, progress ProgressFunc) (*JobResult, error) {
	if progress == nil {
		progress = func(string) {}
	}

	// 1. Create instance
	progress("creating instance")
	inst, err := client.CreateInstance(offer.ID, opts)
	if err != nil {
		return nil, fmt.Errorf("create instance: %w", err)
	}
	instanceID := inst.ID

	// Always destroy on exit
	defer func() {
		progress("destroying instance")
		_ = client.DestroyInstance(instanceID)
	}()

	// 2. Wait for instance to be ready
	progress("waiting for instance")
	inst, err = client.WaitReady(instanceID, 5*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("wait ready: %w", err)
	}

	sshTarget := fmt.Sprintf("root@%s", inst.SSHHost)
	sshPort := strconv.Itoa(inst.SSHPort)
	sshOpts := []string{"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-p", sshPort}

	// 3. Rsync working directory to instance
	if workDir != "" {
		progress("syncing working directory")
		if err := rsyncTo(workDir, sshTarget, "/workspace/", sshPort); err != nil {
			return nil, fmt.Errorf("rsync working dir: %w", err)
		}
	}

	// 4. Rsync input assets (if any declared)
	for _, input := range inputs {
		progress(fmt.Sprintf("syncing input: %s", input))
		// Input assets are paths on the local machine; sync them to /data/ on instance
		if err := rsyncTo(input, sshTarget, "/data/", sshPort); err != nil {
			return nil, fmt.Errorf("rsync input %s: %w", input, err)
		}
	}

	// 5. Install dependencies
	progress("installing dependencies")
	installCmd := "cd /workspace && uv sync 2>&1"
	if _, err := SSHRun(sshTarget, sshOpts, installCmd); err != nil {
		// Non-fatal: uv might not be needed, or project might not use it
		// The onstart-cmd should have installed uv
	}

	// 6. Run the job command
	progress("running job")
	startTime := time.Now()
	jobCmd := fmt.Sprintf("cd /workspace && %s", command)
	output, err := SSHRun(sshTarget, sshOpts, jobCmd)
	runtime := time.Since(startTime)

	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return nil, fmt.Errorf("run job: %w", err)
		}
	}

	// 7. Rsync outputs back
	progress("collecting outputs")
	// For now, rsync the whole workspace back (later: only declared outputs)
	if workDir != "" {
		if syncErr := rsyncFrom(sshTarget, "/workspace/", workDir, sshPort); syncErr != nil {
			// Log but don't fail — we still have the exit code and logs
			output += fmt.Sprintf("\n[weft] warning: failed to sync outputs: %v\n", syncErr)
		}
	}

	// 8. Calculate cost
	totalTime := time.Since(startTime) // includes sync time
	actualCost := totalTime.Hours() * inst.CostPerHour

	return &JobResult{
		ExitCode:   exitCode,
		Stdout:     output,
		ActualCost: actualCost,
		Runtime:    runtime,
	}, nil
}

// LaunchResult contains the instance info from a fire-and-forget launch.
type LaunchResult struct {
	InstanceID int
}

// LaunchJobOnInstance creates a Vast.ai instance, syncs files, deploys the
// wrapper script, and starts it via nohup. It returns immediately after the
// wrapper is started — the instance will upload results to R2 and self-destruct.
//
// The caller must persist the instance ID to the job record BEFORE calling this,
// so that the instance can be recovered if the TUI dies.
func LaunchJobOnInstance(client VastaiClient, offer Offer, opts CreateOpts, workDir string, command string, inputs []string, jobID int64, r2Cfg R2Config, progress ProgressFunc) (*LaunchResult, error) {
	if progress == nil {
		progress = func(string) {}
	}

	// 1. Create instance
	progress("creating instance")
	inst, err := client.CreateInstance(offer.ID, opts)
	if err != nil {
		return nil, fmt.Errorf("create instance: %w", err)
	}
	instanceID := inst.ID

	// 2. Wait for instance to be ready
	progress("waiting for instance")
	inst, err = client.WaitReady(instanceID, 5*time.Minute)
	if err != nil {
		// Destroy on setup failure
		_ = client.DestroyInstance(instanceID)
		return nil, fmt.Errorf("wait ready: %w", err)
	}

	sshTarget := fmt.Sprintf("root@%s", inst.SSHHost)
	sshPort := strconv.Itoa(inst.SSHPort)
	sshOpts := []string{"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-p", sshPort}

	// 3. Rsync working directory to instance
	if workDir != "" {
		progress("syncing working directory")
		if err := rsyncTo(workDir, sshTarget, "/workspace/", sshPort); err != nil {
			_ = client.DestroyInstance(instanceID)
			return nil, fmt.Errorf("rsync working dir: %w", err)
		}
	}

	// 4. Rsync input assets
	for _, input := range inputs {
		progress(fmt.Sprintf("syncing input: %s", input))
		if err := rsyncTo(input, sshTarget, "/data/", sshPort); err != nil {
			_ = client.DestroyInstance(instanceID)
			return nil, fmt.Errorf("rsync input %s: %w", input, err)
		}
	}

	// 5. Write rclone config for R2 access
	progress("configuring R2")
	rcloneConf := GenerateRcloneConfig(r2Cfg)
	setupCmd := fmt.Sprintf("mkdir -p ~/.config/rclone && cat > ~/.config/rclone/rclone.conf << 'RCLONE_EOF'\n%sRCLONE_EOF", rcloneConf)
	if _, err := SSHRun(sshTarget, sshOpts, setupCmd); err != nil {
		_ = client.DestroyInstance(instanceID)
		return nil, fmt.Errorf("write rclone config: %w", err)
	}

	// 6. Deploy wrapper script
	progress("deploying wrapper")
	wrapper := GenerateWrapper(jobID, command, r2Cfg.Bucket)
	deployCmd := fmt.Sprintf("cat > /workspace/.weft-runner.sh << 'WRAPPER_EOF'\n%sWRAPPER_EOF\nchmod +x /workspace/.weft-runner.sh", wrapper)
	if _, err := SSHRun(sshTarget, sshOpts, deployCmd); err != nil {
		_ = client.DestroyInstance(instanceID)
		return nil, fmt.Errorf("deploy wrapper: %w", err)
	}

	// 7. Start wrapper via nohup (fire-and-forget)
	progress("starting job")
	startCmd := fmt.Sprintf("nohup bash /workspace/.weft-runner.sh %d </dev/null >/dev/null 2>&1 &", jobID)
	if _, err := SSHRun(sshTarget, sshOpts, startCmd); err != nil {
		_ = client.DestroyInstance(instanceID)
		return nil, fmt.Errorf("start wrapper: %w", err)
	}

	return &LaunchResult{InstanceID: instanceID}, nil
}

// rsyncTo syncs a local directory to a remote path via SSH.
func rsyncTo(localPath, sshTarget, remotePath, port string) error {
	// Ensure trailing slash for directory contents
	if !strings.HasSuffix(localPath, "/") {
		localPath += "/"
	}
	rshArg := fmt.Sprintf("ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -p %s", port)
	cmd := exec.Command("rsync", "-az", "--delete",
		"-e", rshArg,
		localPath,
		fmt.Sprintf("%s:%s", sshTarget, remotePath),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("rsync: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// rsyncFrom syncs a remote directory to a local path via SSH.
func rsyncFrom(sshTarget, remotePath, localPath, port string) error {
	rshArg := fmt.Sprintf("ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -p %s", port)
	cmd := exec.Command("rsync", "-az",
		"-e", rshArg,
		fmt.Sprintf("%s:%s", sshTarget, remotePath),
		localPath,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("rsync: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// SSHRun executes a command on a remote host via SSH.
func SSHRun(target string, sshOpts []string, command string) (string, error) {
	args := append([]string{}, sshOpts...)
	args = append(args, target, command)
	cmd := exec.Command("ssh", args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
