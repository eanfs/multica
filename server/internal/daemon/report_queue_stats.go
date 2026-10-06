package daemon

import (
	"errors"
	"os"
	"path/filepath"
)

// ReportQueueStats is maintenance evidence, independent of daemon liveness.
type ReportQueueStats struct {
	Known   bool `json:"known"`
	Pending int  `json:"pending"`
	Failed  int  `json:"failed"`
}

// ScanReportQueueStats never initializes, replays or adopts an outbox.
func ScanReportQueueStats(workspacesRoot string) (ReportQueueStats, error) {
	unknown := ReportQueueStats{}
	if !filepath.IsAbs(workspacesRoot) {
		return unknown, errors.New("invalid report root")
	}
	if _, err := reportStatsDirectory(workspacesRoot, false); err != nil {
		return unknown, err
	}
	root := workspacesRoot
	for _, name := range []string{".pending-terminal-reports", "v1"} {
		root = filepath.Join(root, name)
		exists, err := reportStatsDirectory(root, true)
		if err != nil {
			return unknown, err
		}
		if !exists {
			return ReportQueueStats{Known: true}, nil
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return unknown, err
	}
	stats := ReportQueueStats{Known: true}
	for _, entry := range entries {
		// Reject linked namespaces rather than silently proving an empty queue.
		if entry.Type()&os.ModeSymlink != 0 {
			return unknown, errors.New("linked report namespace")
		}
		// As in otherNamespaceStats, only immediate directory namespaces are traversed.
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		if _, err := reportStatsDirectory(dir, false); err != nil {
			return unknown, err
		}
		failed := filepath.Join(dir, "failed")
		if _, err := reportStatsDirectory(failed, true); err != nil {
			return unknown, err
		}
		pending, _, pendingErr := terminalReportDirectoryStats(dir)
		failedCount, _, failedErr := terminalReportDirectoryStats(failed)
		if err := errors.Join(pendingErr, failedErr); err != nil {
			return unknown, err
		}
		stats.Pending += pending
		stats.Failed += failedCount
	}
	return stats, nil
}

// A missing report subdirectory can mean zero; a missing or linked root cannot.
func reportStatsDirectory(path string, allowMissing bool) (bool, error) {
	info, err := os.Lstat(path)
	if allowMissing && errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, errors.New("invalid report directory")
	}
	return true, nil
}
