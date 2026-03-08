//go:build linux

package connkill

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// KillConnectionsAllNamespaces enumerates all unique network namespaces on the host
// and destroys TCP connections to the given destination IP in each one.
// Returns the total number of connections killed and total per-socket destroy errors.
func KillConnectionsAllNamespaces(destIP net.IP) (killed, errCount int, err error) {
	namespaces, err := enumerateNetworkNamespaces()
	if err != nil {
		return 0, 0, fmt.Errorf("enumerating network namespaces: %w", err)
	}

	for _, nsPath := range namespaces {
		nsKilled, nsErrors, nsErr := killInNamespace(nsPath, destIP)
		killed += nsKilled
		errCount += nsErrors
		if nsErr != nil {
			errCount++
		}
	}

	return killed, errCount, nil
}

// enumerateNetworkNamespaces walks /proc/*/ns/net and returns unique namespace paths
// deduplicated by inode.
func enumerateNetworkNamespaces() ([]string, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("reading /proc: %w", err)
	}

	seenInodes := make(map[uint64]bool)
	var namespaces []string

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		// Only process numeric (PID) directories
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}

		nsPath := filepath.Join("/proc", entry.Name(), "ns", "net")
		var stat unix.Stat_t
		if err := unix.Stat(nsPath, &stat); err != nil {
			continue
		}

		if seenInodes[stat.Ino] {
			continue
		}
		seenInodes[stat.Ino] = true
		namespaces = append(namespaces, nsPath)
	}

	return namespaces, nil
}

// killInNamespace enters a network namespace and kills connections to destIP.
func killInNamespace(nsPath string, destIP net.IP) (killed, errCount int, err error) {
	// Lock this goroutine to an OS thread (required for setns)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Save current network namespace
	origNS, err := unix.Open("/proc/self/ns/net", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, 0, fmt.Errorf("opening current netns: %w", err)
	}
	defer func() { _ = unix.Close(origNS) }()

	// Open target namespace
	targetNS, err := unix.Open(nsPath, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, 0, fmt.Errorf("opening target netns %s: %w", nsPath, err)
	}
	defer func() { _ = unix.Close(targetNS) }()

	// Enter target namespace
	if err := unix.Setns(targetNS, unix.CLONE_NEWNET); err != nil {
		// Surface permission failures so callers don't ACK success when no kill happened.
		if isPermissionError(err) {
			return 0, 0, fmt.Errorf("permission denied entering netns %s: %w", nsPath, err)
		}
		return 0, 0, fmt.Errorf("setns to %s: %w", nsPath, err)
	}

	// Kill connections in this namespace
	killed, errCount, killErr := DestroySocketsByDestIP(destIP)

	// Always restore original namespace
	if err := unix.Setns(origNS, unix.CLONE_NEWNET); err != nil {
		return killed, errCount, fmt.Errorf("restoring original netns: %w", err)
	}

	return killed, errCount, killErr
}

func isPermissionError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "permission denied")
}
