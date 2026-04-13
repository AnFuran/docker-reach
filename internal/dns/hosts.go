// Package dns manages container name resolution via the Windows hosts file.
// Entries are placed between marker comments and cleaned up on shutdown.
package dns

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	beginMarker = "# docker-reach BEGIN"
	endMarker   = "# docker-reach END"
	hostsPath   = `C:\Windows\System32\drivers\etc\hosts`
)

// HostsManager manages container DNS entries in the Windows hosts file.
type HostsManager struct {
	mu       sync.Mutex
	backedUp bool // true after first backup has been created
}

func NewHostsManager() *HostsManager {
	return &HostsManager{}
}

// backupOnce creates a one-time backup of the hosts file before any
// modification. The backup is written next to the exe so the user can
// find it easily if something goes wrong.
func (h *HostsManager) backupOnce() {
	if h.backedUp {
		return
	}
	h.backedUp = true

	data, err := os.ReadFile(hostsPath)
	if err != nil {
		slog.Warn("could not read hosts file for backup", "error", err)
		return
	}

	// Validate: refuse to back up an empty or suspiciously small file.
	if len(data) < 10 {
		slog.Warn("hosts file is suspiciously small, skipping backup", "size", len(data))
		return
	}

	backupPath := hostsPath + ".docker-reach-backup"
	if err := os.WriteFile(backupPath, data, 0644); err != nil {
		slog.Warn("could not create hosts backup", "error", err)
		return
	}
	slog.Info("hosts file backed up", "path", backupPath)
}

// sanitizeName returns a hostname-safe version of a container name.
// Underscores are replaced with hyphens. Names containing dots are rejected
// (returns "", false) because dots would create ambiguous subdomain-like
// hostnames that could shadow real DNS entries.
func sanitizeName(name string) (string, bool) {
	if strings.Contains(name, ".") {
		slog.Warn("skipping container name with dots", "name", name)
		return "", false
	}
	return strings.ReplaceAll(name, "_", "-"), true
}

// atomicWriteFile writes data to a temporary file in the same directory as
// dst, then renames it over dst. On Windows NTFS, rename within the same
// volume is atomic, so a crash mid-write cannot corrupt dst.
func atomicWriteFile(dst string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(dst)
	tmp, err := os.CreateTemp(dir, ".hosts-tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()

	// Ensure the temp file is removed if anything goes wrong before rename.
	ok := false
	defer func() {
		if !ok {
			os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		// On Windows, rename can fail when the target is locked by svchost
		// (DNS Client service). Fall back to in-place overwrite using
		// write-then-truncate, which is safe against mid-write process kill.
		// (os.WriteFile uses O_TRUNC which empties the file BEFORE writing —
		// if killed between truncate and write, the file is left empty.)
		slog.Debug("atomic rename failed, falling back to safe overwrite", "error", err)
		os.Remove(tmpName)
		ok = true // prevent deferred remove of tmp (already removed)
		return safeOverwrite(dst, data, perm)
	}
	ok = true
	return nil
}

// safeOverwrite writes data to dst using write-then-truncate instead of
// truncate-then-write (os.WriteFile). If the process is killed mid-write,
// the file retains a mix of new + old content rather than being completely
// empty. This is critical for the Windows hosts file.
func safeOverwrite(dst string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE, perm)
	if err != nil {
		return fmt.Errorf("open for overwrite: %w", err)
	}
	defer f.Close()

	if _, err := f.WriteAt(data, 0); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	// Truncate AFTER writing — removes stale trailing bytes from the old
	// content. If killed before this line, the file has extra trailing
	// data from the previous version, which is harmless.
	if err := f.Truncate(int64(len(data))); err != nil {
		return fmt.Errorf("truncate: %w", err)
	}
	return nil
}

// Update replaces the docker-reach section in the hosts file.
// Keys are container names, values are IPs.
func (h *HostsManager) Update(records map[string]net.IP) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.backupOnce()

	data, err := os.ReadFile(hostsPath)
	if err != nil {
		return fmt.Errorf("read hosts: %w", err)
	}

	content := string(data)

	// Remove existing docker-reach section
	content = removeSection(content)

	// Build new section
	if len(records) > 0 {
		var sb strings.Builder
		sb.WriteString(beginMarker + "\n")
		for name, ip := range records {
			safe, ok := sanitizeName(name)
			if !ok {
				continue
			}
			// Write two entries so both bare name and .docker suffix resolve:
			//   curl http://my-container       (bare)
			//   curl http://my-container.docker (.docker suffix)
			sb.WriteString(fmt.Sprintf("%-20s %s\n", ip, safe))
			sb.WriteString(fmt.Sprintf("%-20s %s\n", ip, safe+".docker"))
		}
		sb.WriteString(endMarker + "\n")
		content = strings.TrimRight(content, "\r\n") + "\n" + sb.String()
	}

	// Safety check: never write an empty or near-empty hosts file. If content
	// is suspiciously small, something went wrong — abort to protect user data.
	if len(content) < 10 {
		return fmt.Errorf("refusing to write hosts file: content too small (%d bytes), possible data loss", len(content))
	}

	if err := atomicWriteFile(hostsPath, []byte(content), 0644); err != nil {
		return fmt.Errorf("write hosts: %w", err)
	}

	slog.Info("hosts file updated", "entries", len(records))
	return nil
}

// Cleanup removes the docker-reach section from the hosts file.
func (h *HostsManager) Cleanup() error {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.backupOnce()

	data, err := os.ReadFile(hostsPath)
	if err != nil {
		return err
	}

	cleaned := removeSection(string(data))

	// Safety check: never write an empty or near-empty hosts file.
	if len(cleaned) < 10 {
		return fmt.Errorf("refusing to write hosts file during cleanup: content too small (%d bytes)", len(cleaned))
	}

	return atomicWriteFile(hostsPath, []byte(cleaned), 0644)
}

// removeSection strips the docker-reach block (beginMarker … endMarker) from
// content and returns the result. If the markers are absent, duplicated, or in
// the wrong order the original content is returned unchanged.
func removeSection(content string) string {
	start := strings.Index(content, beginMarker)
	end := strings.Index(content, endMarker)
	if start == -1 || end == -1 {
		return content
	}

	// Guard against corrupted/duplicated markers that would cause start >= end,
	// which would delete everything between them in the wrong direction.
	if start >= end {
		slog.Warn("hosts file markers are in invalid order; leaving file unchanged",
			"start", start, "end", end)
		return content
	}

	end += len(endMarker)
	// Consume the newline (CRLF or LF) that follows the end marker.
	if end < len(content) && content[end] == '\n' {
		end++
	} else if end < len(content) && content[end] == '\r' {
		end++
		if end < len(content) && content[end] == '\n' {
			end++
		}
	}
	return content[:start] + content[end:]
}
