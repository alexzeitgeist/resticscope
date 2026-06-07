//go:build linux || darwin

package app

// extract_metadata.go is the v1 directory-metadata normalization gate
// (00-framework.md §18). restic 0.18.1 exposes no restore flag to suppress
// ownership/xattr/mode restoration, so after a successful live tree restore — and
// before the staging dir is renamed to final — this pass walks the staging tree
// and forces every entry to safe, owner-only metadata:
//
//   - regular files → 0600, directories → 0700. A directory is forced
//     owner-traversable before its children are read (restic restores the
//     snapshot's original mode, which may lack owner r/x) and is otherwise
//     normalized post-order; suid/sgid/sticky are dropped by replacing the whole
//     permission word.
//   - ownership reset to the effective uid/gid (a no-op when already correct;
//     a hard failure when it differs and cannot be changed).
//   - xattrs/ACLs removed via the l* (no-follow) calls; a filesystem that reports
//     xattrs unsupported — or a single attribute it refuses to remove — is
//     skipped and counted, not failed (documented v1 tolerance).
//   - mtimes normalized to one baseline instant, including symlinks (via the
//     no-follow unix.Lutimes; the link target is never touched).
//   - symlinks kept only when their target is relative and cannot escape the
//     extracted tree after lexical cleaning; never followed, never chmod'd, but
//     their own timestamps are reset like every other node.
//   - devices/fifos/sockets and any other node type rejected.
//
// Every failure returns a path-free ErrExtractMetadataNormalization; the caller
// leaves staging in place and does not rename.

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// extractMetaCounts tallies what the normalizer changed. It is count-only — it
// holds no paths — so it is safe to log.
type extractMetaCounts struct {
	Files         int // regular files normalized
	Dirs          int // directories normalized (excludes the staging root)
	XattrsRemoved int
	Skipped       int // xattrs left in place because the fs reported them unsupported
}

// normalizeExtractTreeMetadata walks root (the staging dir) and normalizes every
// entry's metadata. It follows no symlinks and returns a path-free error on the
// first failure.
func normalizeExtractTreeMetadata(ctx context.Context, root string, baseline time.Time) (extractMetaCounts, error) {
	var counts extractMetaCounts
	euid := os.Geteuid()
	egid := os.Getegid()
	err := normalizeExtractNode(ctx, root, root, baseline, euid, egid, true, &counts)
	return counts, err
}

// normalizeExtractNode normalizes one entry, recursing into directories
// post-order. isRoot suppresses counting the staging dir itself as a normalized
// directory. A ctx error is propagated verbatim so a timeout/cancel during the
// walk is not misreported as a metadata failure.
func normalizeExtractNode(ctx context.Context, root, p string, baseline time.Time, euid, egid int, isRoot bool, counts *extractMetaCounts) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	fi, err := os.Lstat(p)
	if err != nil {
		return metaErr(err)
	}
	mode := fi.Mode()
	st, _ := fi.Sys().(*syscall.Stat_t)

	switch {
	case mode&fs.ModeSymlink != 0:
		if err := validateExtractSymlink(root, p); err != nil {
			return err
		}
		if err := chownToEffective(p, st, euid, egid, true); err != nil {
			return err
		}
		if err := removeXattrs(p, counts); err != nil {
			return err
		}
		// Reset the symlink's own timestamps to the baseline like every other
		// node. os.Chtimes would follow the link (and re-time its target); Lutimes
		// is guaranteed no-follow on linux/darwin, the only platforms this gate
		// builds for, so the link's target is never touched.
		tv := unix.NsecToTimeval(baseline.UnixNano())
		if err := unix.Lutimes(p, []unix.Timeval{tv, tv}); err != nil {
			return metaErr(err)
		}
		return nil

	case mode.IsDir():
		// Force the directory owner-traversable before reading it: restic restores
		// the snapshot's original mode, which may lack owner r/x (e.g. 0000, 0500)
		// and would otherwise block ReadDir / child Lstat. Staging is private and
		// the final mode is 0700 regardless, so this is also the canonical mode
		// normalization — nothing below changes the permission bits.
		if err := os.Chmod(p, 0o700); err != nil {
			return metaErr(err)
		}
		entries, err := os.ReadDir(p)
		if err != nil {
			return metaErr(err)
		}
		for _, e := range entries {
			if err := normalizeExtractNode(ctx, root, filepath.Join(p, e.Name()), baseline, euid, egid, false, counts); err != nil {
				return err
			}
		}
		// Post-order: ownership, xattrs, and mtime after the children.
		if err := removeXattrs(p, counts); err != nil {
			return err
		}
		if err := chownToEffective(p, st, euid, egid, false); err != nil {
			return err
		}
		if err := os.Chtimes(p, baseline, baseline); err != nil {
			return metaErr(err)
		}
		if !isRoot {
			counts.Dirs++
		}
		return nil

	case mode.IsRegular():
		if err := removeXattrs(p, counts); err != nil {
			return err
		}
		if err := chownToEffective(p, st, euid, egid, false); err != nil {
			return err
		}
		if err := os.Chmod(p, 0o600); err != nil {
			return metaErr(err)
		}
		if err := os.Chtimes(p, baseline, baseline); err != nil {
			return metaErr(err)
		}
		counts.Files++
		return nil

	default:
		// Devices, fifos, sockets, and anything else cannot be safely published.
		return fmt.Errorf("%w: unsupported node type", ErrExtractMetadataNormalization)
	}
}

// validateExtractSymlink permits a symlink only when its target is non-empty,
// NUL-free, relative, and cannot escape root after lexical cleaning. The target
// is never followed.
func validateExtractSymlink(root, p string) error {
	target, err := os.Readlink(p)
	if err != nil {
		return metaErr(err)
	}
	if target == "" || strings.ContainsRune(target, '\x00') {
		return fmt.Errorf("%w: unsafe symlink target", ErrExtractMetadataNormalization)
	}
	if filepath.IsAbs(target) {
		return fmt.Errorf("%w: absolute symlink target", ErrExtractMetadataNormalization)
	}
	resolved := filepath.Clean(filepath.Join(filepath.Dir(p), target))
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: escaping symlink target", ErrExtractMetadataNormalization)
	}
	return nil
}

// chownToEffective resets ownership to euid/egid. It is a no-op when the entry is
// already owned correctly; otherwise it Lchown/Chowns and fails the extract if it
// cannot. The failure message is path-free.
func chownToEffective(p string, st *syscall.Stat_t, euid, egid int, isSymlink bool) error {
	if st != nil && int(st.Uid) == euid && int(st.Gid) == egid {
		return nil
	}
	var err error
	if isSymlink {
		err = os.Lchown(p, euid, egid)
	} else {
		err = os.Chown(p, euid, egid)
	}
	if err != nil {
		return fmt.Errorf("%w: cannot normalize ownership", ErrExtractMetadataNormalization)
	}
	return nil
}

// removeXattrs strips every extended attribute (POSIX ACLs ride here too) from p
// using the no-follow calls. A filesystem that reports xattrs unsupported is
// counted as skipped rather than failed.
func removeXattrs(p string, counts *extractMetaCounts) error {
	names, err := listXattrNames(p)
	if err != nil {
		if isXattrUnsupported(err) {
			counts.Skipped++
			return nil
		}
		return metaErr(err)
	}
	for _, name := range names {
		if err := unix.Lremovexattr(p, name); err != nil {
			switch {
			case isXattrUnsupported(err):
				// The filesystem listed the attribute but refuses to remove it
				// (rare; e.g. an immutable system/namespace attr). v1 policy is to
				// tolerate it: count it as skipped rather than fail the whole
				// extract. A surviving xattr here is an accepted, documented v1
				// trade-off, not a normalization failure.
				counts.Skipped++
				continue
			case err == unix.ENODATA:
				// Vanished between list and remove — already gone, nothing to count.
				continue
			default:
				return metaErr(err)
			}
		}
		counts.XattrsRemoved++
	}
	return nil
}

// listXattrNames returns the NUL-separated xattr names on p (no symlink follow).
func listXattrNames(p string) ([]string, error) {
	sz, err := unix.Llistxattr(p, nil)
	if err != nil {
		return nil, err
	}
	if sz == 0 {
		return nil, nil
	}
	buf := make([]byte, sz)
	sz, err = unix.Llistxattr(p, buf)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, b := range bytes.Split(buf[:sz], []byte{0}) {
		if len(b) > 0 {
			names = append(names, string(b))
		}
	}
	return names, nil
}

// isXattrUnsupported reports whether err means the filesystem does not support
// xattrs (so removal is a no-op, not a failure).
func isXattrUnsupported(err error) bool {
	return err == unix.ENOTSUP || err == unix.EOPNOTSUPP
}

// metaErr wraps a filesystem failure as ErrExtractMetadataNormalization with the
// path stripped.
func metaErr(err error) error {
	return fmt.Errorf("%w: %v", ErrExtractMetadataNormalization, pathFreeCause(err))
}
