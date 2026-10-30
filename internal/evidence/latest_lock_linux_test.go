//go:build linux

package evidence

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/redhat-appstudio/konflux-test/internal/model"
	"golang.org/x/sys/unix"
)

const (
	latestLockRootEnv = "KONFLUX_TEST_LATEST_LOCK_ROOT"
	latestLockRunEnv  = "KONFLUX_TEST_LATEST_LOCK_RUN"
)

// TestSetLatestWaitsForCrossProcessLock verifies that concurrent latest updates wait for the file lock.
func TestSetLatestWaitsForCrossProcessLock(t *testing.T) {
	if root := os.Getenv(latestLockRootEnv); root != "" {
		fd, err := unix.Open(filepath.Join(root, ".latest.lock"), unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = unix.Close(fd) }()
		if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err == nil {
			_ = unix.Flock(fd, unix.LOCK_UN)
			t.Fatal("non-blocking lock unexpectedly acquired while parent holds the lock")
		} else if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			t.Fatalf("non-blocking lock returned %v, want EWOULDBLOCK", err)
		}
		if err := os.WriteFile(filepath.Join(root, "child-contended"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		store := NewManifestStore(root)
		runID := os.Getenv(latestLockRunEnv)
		manifest, err := store.Load(runID)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.SetLatest(manifest); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "child-complete"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}

	root := t.TempDir()
	store := NewManifestStore(root)
	baseline := model.RunManifest{RunID: "baseline", ArtifactDirectory: "baseline", CreatedAt: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)}
	if err := store.Create(&baseline); err != nil {
		t.Fatal(err)
	}
	candidate := model.RunManifest{RunID: "candidate", ArtifactDirectory: "candidate", CreatedAt: baseline.CreatedAt.Add(time.Minute)}
	if err := os.MkdirAll(store.RunDirFor(candidate), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(&candidate); err != nil {
		t.Fatal(err)
	}

	lockFile, err := os.OpenFile(filepath.Join(root, ".latest.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lockFile.Close() }()
	if err := unix.Flock(int(lockFile.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	locked := true
	defer func() {
		if locked {
			_ = unix.Flock(int(lockFile.Fd()), unix.LOCK_UN)
		}
	}()

	command := exec.Command(os.Args[0], "-test.run=^TestSetLatestWaitsForCrossProcessLock$", "-test.v")
	for _, value := range os.Environ() {
		if strings.HasPrefix(value, latestLockRootEnv+"=") || strings.HasPrefix(value, latestLockRunEnv+"=") {
			continue
		}
		command.Env = append(command.Env, value)
	}
	command.Env = append(command.Env, latestLockRootEnv+"="+root, latestLockRunEnv+"="+candidate.RunID)
	var childOutput bytes.Buffer
	command.Stdout = &childOutput
	command.Stderr = &childOutput
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { finished <- command.Wait() }()

	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		if _, err := os.Stat(filepath.Join(root, "child-contended")); err == nil {
			break
		}
		select {
		case err := <-finished:
			t.Fatalf("child exited before confirming lock contention: %v; output: %s", err, childOutput.String())
		case <-deadline.C:
			t.Fatal("child did not confirm lock contention")
		case <-time.After(10 * time.Millisecond):
		}
	}

	waiterDeadline := time.Now().Add(5 * time.Second)
	for {
		waiting, err := flockWaiterPresent(command.Process.Pid)
		if err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-finished:
			t.Fatalf("SetLatest completed without waiting on the cross-process lock: %v; output: %s", err, childOutput.String())
		default:
		}
		if time.Now().After(waiterDeadline) {
			t.Fatal("child did not enter the kernel flock wait queue")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := unix.Flock(int(lockFile.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	locked = false
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("child remained blocked after lock release")
	}
	if _, err := os.Stat(filepath.Join(root, "child-complete")); err != nil {
		t.Fatalf("child did not complete latest update: %v", err)
	}
	latest, err := store.Latest()
	if err != nil {
		t.Fatal(err)
	}
	if latest != candidate.RunID {
		t.Fatalf("latest run = %q, want %q", latest, candidate.RunID)
	}
}

// flockWaiterPresent reports whether pid is queued for a flock lock in /proc/locks.
func flockWaiterPresent(pid int) (bool, error) {
	data, err := os.ReadFile("/proc/locks")
	if err != nil {
		return false, err
	}
	processID := strconv.Itoa(pid)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 5 && fields[1] == "->" && fields[2] == "FLOCK" && fields[5] == processID {
			return true, nil
		}
	}
	return false, nil
}
