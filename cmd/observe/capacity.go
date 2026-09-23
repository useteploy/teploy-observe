package main

import (
	"os"
	"syscall"
)

// O12 disk-pressure surface: /healthz carries the filesystem posture of
// every data path observe can see locally. The ENGINE's data directory is
// only visible when it runs on the same host and the operator points
// OBSERVE_NUCLEUS_DATA_DIR at it — when unset, the health block says so
// explicitly instead of implying the engine has no disk usage (engine-side
// disk reporting is an upstream capability ask, recorded in AUDIT_OPEN).

// diskView reports the usage of one path in the /healthz shape. A path
// that cannot be statfs'd reports ok=false with the error, never a
// silent zero.
func diskView(path string) map[string]any {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return map[string]any{
			"path": path,
			"ok":   false,
			"error": err.Error(),
		}
	}
	// Bavail (root-reserved excluded) is the honest "how much can still
	// be written"; Blocks x Bsize is the filesystem size.
	free := uint64(st.Bavail) * uint64(st.Bsize)
	total := uint64(st.Blocks) * uint64(st.Bsize)
	usedPct := 0.0
	if total > 0 {
		usedPct = float64(total-free) / float64(total) * 100
	}
	return map[string]any{
		"path":        path,
		"ok":          true,
		"free_bytes":  free,
		"total_bytes": total,
		"used_pct":    usedPct,
	}
}

// capacityView assembles the coherent capacity block for /healthz: the
// process's own data dir, the engine's data dir where co-located and
// declared, and the query-admission snapshot (a queryguard.Snapshot —
// the JSON encoder renders its counters/budgets directly).
func capacityView(dataDir string, querySnap any) map[string]any {
	block := map[string]any{
		"disk":  diskView(dataDir),
		"query": querySnap,
	}
	if engineDir := os.Getenv("OBSERVE_NUCLEUS_DATA_DIR"); engineDir != "" {
		block["engine_disk"] = diskView(engineDir)
	} else {
		block["engine_disk"] = map[string]any{
			"ok":     false,
			"reason": "engine data path not co-located or not declared - set OBSERVE_NUCLEUS_DATA_DIR when the engine's data directory is on this host",
		}
	}
	return block
}
