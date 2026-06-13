//go:build linux || darwin

package app

// extract_metadata.go is the post-restore staging pass for a live tree extract.
// restic restores a faithful, verbatim copy of the snapshot — original modes
// (including suid/sgid/sticky), mtimes, xattrs/ACLs, and ownership — and that is
// exactly what an extract should publish: every tool in the ecosystem (tar,
// bsdtar, borg, cpio, rsync) treats a verbatim restore as the norm. So this pass
// preserves everything intrinsic to a file and never rewrites it.
//
// The one exception is the symlink target, the only *positional* piece of
// metadata: once a fragment lands in a foreign scratch dir, an absolute or
// tree-escaping link silently aliases the *live* filesystem (a read returns
// current data, not backup data; a write through it clobbers a live file). Such
// links are classified "unsafe" and handled per the [extract] unsafe_symlinks
// policy — keep + warn (default, matching restic), skip, or placeholder.
//
// Crucially the pass NEVER aborts the whole tree on classification: unsafe
// symlinks and device/fifo/socket nodes are counted and (for symlinks) acted on,
// never treated as a fatal error — the old v1 gate's symlink/special-file abort
// tripped on common /etc subtrees and yielded nothing. Only a genuine IO error
// hard-fails, and every such failure returns a path-free metaErr with staging
// retained for the caller's keep-or-delete:
//
//   - Lstat / ReadDir / Chmod under any policy, and
//   - Remove / WriteFile under the mutating skip/placeholder policies.
//
// So the mutating policies carry strictly more failure surface than keep. The
// pass follows no symlink. The only filesystem mutations on the preserved path
// are the directory temp-chmod-and-restore used to traverse a dir restic left
// without owner rwx (see normalizeExtractDir).

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// liveExtractSupported reports whether an extract can publish on this platform.
// The post-restore metadata normalizer is validated only on linux/darwin, and
// the single-file --include escaping is not Windows-safe, so an extract is
// refused elsewhere by an app-level preflight (App.Extract). It is a var, not a
// const, so a linux/darwin test can flip it to exercise the refusal path. Its
// sibling in extract_metadata_other.go is false.
var liveExtractSupported = true

// normalizeExtractTreeMetadata walks root (the staging dir) and reproduces
// restic's restored metadata for every intrinsic file attribute, deviating only
// on unsafe symlinks per policy. It follows no symlinks, never aborts on node
// classification, and returns a path-free error only on a genuine IO failure.
// The root is the staging dir App.Extract just created, so it is always a
// directory; its Lstat here is the only full stat the walk needs for free —
// children are dispatched on the ReadDir entry type (no per-node re-stat).
func normalizeExtractTreeMetadata(ctx context.Context, root string, policy unsafeSymlinkPolicy) (extractMetaCounts, error) {
	var counts extractMetaCounts
	fi, err := os.Lstat(root)
	if err != nil {
		return counts, metaErr(err)
	}
	err = normalizeExtractDir(ctx, root, root, fi.Mode(), policy, true, &counts)
	return counts, err
}

// normalizeExtractChild handles one directory entry, recursing into
// subdirectories. It dispatches on the ReadDir-provided entry type, so only
// directories — which need the full mode for the temp-chmod check — pay an extra
// stat; symlink/regular/other nodes cost no syscall beyond the parent's ReadDir.
// Trusting e.Type() is safe even on filesystems whose readdir reports
// DT_UNKNOWN (NFS/FUSE): os.ReadDir resolves that sentinel itself with an
// internal lstat before the DirEntry reaches the caller (os/file_unix.go,
// newUnixDirent), so the type seen here is never ambiguous. A ctx error is
// propagated verbatim so a timeout/cancel during the walk is not misreported as
// a metadata failure.
func normalizeExtractChild(ctx context.Context, root, dir string, e fs.DirEntry, policy unsafeSymlinkPolicy, counts *extractMetaCounts) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p := filepath.Join(dir, e.Name())
	typ := e.Type()

	switch {
	case typ&fs.ModeSymlink != 0:
		// Ordered before IsDir(): a symlink is never followed or recursed.
		unsafe, target, err := classifyExtractSymlink(root, p)
		if err != nil {
			return err
		}
		if !unsafe {
			return nil // safe link: leave verbatim
		}
		counts.UnsafeSymlinks++
		return applyUnsafeSymlinkPolicy(p, target, policy)

	case typ.IsDir():
		fi, err := e.Info()
		if err != nil {
			return metaErr(err)
		}
		return normalizeExtractDir(ctx, root, p, fi.Mode(), policy, false, counts)

	case typ.IsRegular():
		// Mode, mtime, xattrs, and ownership are kept exactly as restic restored
		// them — nothing to do but count.
		counts.Files++
		return nil

	default:
		// Devices, fifos, sockets, and anything else: counted and left in place,
		// never an abort.
		counts.Other++
		return nil
	}
}

// normalizeExtractDir reads p's children and recurses, restoring p's original
// mode afterward. restic restores the snapshot's original directory mode, which
// may lack owner r/w/x (e.g. 0000, 0500); the pass forces the directory
// owner-rwx before reading it and restores the original mode after.
//
// The mask is 0o700 (owner rwx), not 0o500 (owner r-x): read+execute is enough to
// *traverse*, but skip/placeholder mutate *children* (Remove/WriteFile), which
// needs *write* on the parent dir — a 0500 dir is traversable yet not writable,
// so an r+x-only check would let those policies fail with EACCES. In practice
// this only bites under root, since a non-root restic leaves staging owned by the
// running user.
//
// The restore uses the original full os.FileMode (not .Perm()) so setgid/sticky
// on directories survive — .Perm() would silently strip them and reintroduce the
// metadata clobbering this rewrite exists to remove. It is deferred immediately
// after the temp-chmod so it runs on EVERY exit — a ReadDir/child error, an
// unsafe-link mutation failure, or a ctx-cancel mid-recursion — leaving a staging
// tree retained for keep-or-delete inspection with restic's mode, not the
// temporary widening. A restore failure surfaces as a path-free metaErr only when
// nothing else already failed (the original error is the more informative one).
func normalizeExtractDir(ctx context.Context, root, p string, origMode fs.FileMode, policy unsafeSymlinkPolicy, isRoot bool, counts *extractMetaCounts) (err error) {
	if origMode.Perm()&0o700 != 0o700 {
		if cerr := os.Chmod(p, origMode|0o700); cerr != nil {
			return metaErr(cerr)
		}
		defer func() {
			if rerr := os.Chmod(p, origMode); rerr != nil && err == nil {
				err = metaErr(rerr)
			}
		}()
	}
	entries, rderr := os.ReadDir(p)
	if rderr != nil {
		return metaErr(rderr)
	}
	// The child loop runs between the temp-chmod and the deferred restore, so
	// unsafe-link mutations always see a writable parent.
	for _, e := range entries {
		if cerr := normalizeExtractChild(ctx, root, p, e, policy, counts); cerr != nil {
			return cerr
		}
	}
	if !isRoot {
		counts.Dirs++
	}
	return nil
}

// classifyExtractSymlink reads the link at p and classifies its target via the
// pure classifySymlinkTarget. It returns whether the target is unsafe, the raw
// target string (for the placeholder policy), and a path-free metaErr only on a
// Readlink IO failure. The link is never followed.
func classifyExtractSymlink(root, p string) (unsafe bool, target string, err error) {
	target, rerr := os.Readlink(p)
	if rerr != nil {
		return false, "", metaErr(rerr)
	}
	return classifySymlinkTarget(root, filepath.Dir(p), target), target, nil
}

// classifySymlinkTarget reports whether a symlink target is unsafe to publish: a
// target that is empty, NUL-bearing, absolute, or escapes root after lexical
// cleaning aliases something outside the extracted tree (the live filesystem once
// the fragment is in a scratch dir). It is pure — no filesystem access, no link
// follow — so the empty/NUL guards (targets os.Symlink cannot even create) are
// testable directly.
func classifySymlinkTarget(root, parentDir, target string) bool {
	if target == "" || strings.ContainsRune(target, '\x00') {
		return true
	}
	if filepath.IsAbs(target) {
		return true
	}
	resolved := filepath.Clean(filepath.Join(parentDir, target))
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return true
	}
	return false
}

// applyUnsafeSymlinkPolicy acts on an unsafe symlink at p per policy. The switch
// default arm is keep, so an empty or unknown policy string falls through to
// keep (fail-safe, matching restic's verbatim restore). Every Remove/WriteFile
// failure returns a path-free metaErr — a raw *os.PathError would leak p — which
// is why the mutating policies carry more failure surface than keep.
func applyUnsafeSymlinkPolicy(p, target string, policy unsafeSymlinkPolicy) error {
	switch policy {
	case unsafeSymlinkSkip:
		if err := os.Remove(p); err != nil {
			return metaErr(err)
		}
	case unsafeSymlinkPlaceholder:
		if err := os.Remove(p); err != nil {
			return metaErr(err)
		}
		// An inert text file recording the target the link pointed at: the
		// information survives without an alias to the live filesystem.
		if err := os.WriteFile(p, []byte(target+"\n"), 0o600); err != nil {
			return metaErr(err)
		}
	default:
		// keep (and "" / unknown → keep): leave the link verbatim.
	}
	return nil
}

// metaErr wraps a filesystem failure as ErrExtractMetadataNormalization with the
// path stripped.
func metaErr(err error) error {
	return fmt.Errorf("%w: %w", ErrExtractMetadataNormalization, pathFreeCause(err))
}
