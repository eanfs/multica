package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReportQueueStatsAllNamespacesAndFailed(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"current", "foreign"} {
		dir := filepath.Join(root, ".pending-terminal-reports", "v1", name)
		if err := os.MkdirAll(filepath.Join(dir, "failed"), 0700); err != nil {
			t.Fatal(err)
		}
		for _, file := range []string{"pending.json", "not-json", "failed/quarantined"} {
			if err := os.WriteFile(filepath.Join(dir, file), []byte("private payload"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Mkdir(filepath.Join(dir, "ignored-directory"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ScanReportQueueStats(root)
	if err != nil || !got.Known || got.Pending != 4 || got.Failed != 2 {
		t.Fatalf("all namespaces = %+v, %v; want known 4/2", got, err)
	}
}

func TestReportQueueStatsReadErrorUnknown(t *testing.T) {
	for _, kind := range []string{"missing-root", "symlink-root", "file-root", "file-version", "symlink-reports", "file-failed", "symlink-namespace", "symlink-failed"} {
		t.Run(kind, func(t *testing.T) {
			parent := t.TempDir()
			root := filepath.Join(parent, "workspaces")
			if kind != "missing-root" {
				if err := os.Mkdir(root, 0700); err != nil {
					t.Fatal(err)
				}
			}
			switch kind {
			case "symlink-root":
				root = filepath.Join(parent, "link")
				if err := os.Symlink(filepath.Join(parent, "workspaces"), root); err != nil {
					t.Fatal(err)
				}
			case "file-root":
				root = filepath.Join(parent, "file")
				if err := os.WriteFile(root, nil, 0600); err != nil {
					t.Fatal(err)
				}
			case "file-version":
				if err := os.Mkdir(filepath.Join(root, ".pending-terminal-reports"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, ".pending-terminal-reports", "v1"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink-reports":
				if err := os.Symlink(t.TempDir(), filepath.Join(root, ".pending-terminal-reports")); err != nil {
					t.Fatal(err)
				}
			case "symlink-namespace", "symlink-failed":
				dir := filepath.Join(root, ".pending-terminal-reports", "v1")
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				link := filepath.Join(dir, "foreign")
				if kind == "symlink-failed" {
					if err := os.Mkdir(link, 0700); err != nil {
						t.Fatal(err)
					}
					link = filepath.Join(link, "failed")
				}
				if err := os.Symlink(t.TempDir(), link); err != nil {
					t.Fatal(err)
				}
			case "file-failed":
				dir := filepath.Join(root, ".pending-terminal-reports", "v1", "foreign")
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "failed"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := ScanReportQueueStats(root)
			if err == nil || got.Known {
				t.Fatalf("unsafe root = %+v %v", got, err)
			}
		})
	}
}

func TestReportQueueStatsMissingReportsKnownZeroWithoutWrites(t *testing.T) {
	root := t.TempDir()
	got, err := ScanReportQueueStats(root)
	if err != nil || got != (ReportQueueStats{Known: true}) {
		t.Fatalf("verified empty root: %+v %v", got, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("scanner wrote directories: %v %v", entries, err)
	}
}
