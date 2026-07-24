//go:build linux || darwin

package app

// Metadata normalization preserves restic's modes, times, attributes, and
// ownership, except for unsafe symlink policy. Special nodes are counted rather
// than rejected; cancellation and path-free IO failures abort while retaining
// staging. os.Root confines path operations, and temporary directory permission
// widening is restored after traversal.

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// liveExtractSupported reports whether metadata and include handling are
// validated on this platform. It is mutable so tests can exercise refusal.
var liveExtractSupported = true

// normalizeExtractTreeMetadata preserves staged metadata while applying unsafe
// symlink policy. It follows no links, tolerates all node types, and returns
// path-free IO errors.
func normalizeExtractTreeMetadata(ctx context.Context, root string, policy unsafeSymlinkPolicy) (extractMetaCounts, error) {
	var counts extractMetaCounts
	// Confine every syscall below to staging: os.Root rejects escaping paths, so
	// a dir->symlink swap in restored content cannot redirect a mutation onto the
	// live filesystem.
	rt, err := os.OpenRoot(root)
	if err != nil {
		return counts, metaErr(err)
	}
	defer func() { _ = rt.Close() }()
	fi, err := rt.Lstat(".")
	if err != nil {
		return counts, metaErr(err)
	}
	err = normalizeExtractDir(ctx, rt, root, root, fi.Mode(), policy, true, &counts)
	return counts, err
}

// normalizeExtractChild handles one entry. os.ReadDir resolves DT_UNKNOWN before
// returning entries, including on NFS and FUSE. Context errors remain distinct
// from metadata failures.
func normalizeExtractChild(ctx context.Context, rt *os.Root, root, dir string, e fs.DirEntry, policy unsafeSymlinkPolicy, counts *extractMetaCounts) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p := filepath.Join(dir, e.Name())
	typ := e.Type()

	switch {
	case typ&fs.ModeSymlink != 0:
		// Ordered before IsDir(): a symlink is never followed or recursed.
		unsafe, target, err := classifyExtractSymlink(rt, root, p)
		if err != nil {
			return err
		}
		if !unsafe {
			return nil // safe link: leave verbatim
		}
		counts.UnsafeSymlinks++
		return applyUnsafeSymlinkPolicy(rt, root, p, target, policy)

	case typ.IsDir():
		fi, err := e.Info()
		if err != nil {
			return metaErr(err)
		}
		return normalizeExtractDir(ctx, rt, root, p, fi.Mode(), policy, false, counts)

	case typ.IsRegular():
		// Kept exactly as restic restored it - nothing to do but count.
		counts.Files++
		return nil

	default:
		// Preserve and count special nodes rather than aborting extraction.
		counts.Other++
		return nil
	}
}

// normalizeExtractDir temporarily grants owner rwx for traversal and symlink
// mutation, then restores the full original mode on every exit. restic restores
// the snapshot's own directory mode, which may lack owner r/w/x. The mask is
// 0o700, not 0o500: skip/placeholder Remove and WriteFile children, which needs
// write on the parent, so a traverse-only widening would fail with EACCES. The
// deferred restore preserves setgid and sticky bits and reports a path-free
// error only when no earlier failure exists.
func normalizeExtractDir(ctx context.Context, rt *os.Root, root, p string, origMode fs.FileMode, policy unsafeSymlinkPolicy, isRoot bool, counts *extractMetaCounts) (err error) {
	rel, relErr := filepath.Rel(root, p)
	if relErr != nil {
		return metaErr(relErr)
	}
	if origMode.Perm()&0o700 != 0o700 {
		if cerr := rt.Chmod(rel, origMode|0o700); cerr != nil {
			return metaErr(cerr)
		}
		defer func() {
			if rerr := rt.Chmod(rel, origMode); rerr != nil && err == nil {
				err = metaErr(rerr)
			}
		}()
	}
	entries, rderr := readDirSortedIn(rt, rel)
	if rderr != nil {
		return metaErr(rderr)
	}
	// Run the child loop between the temp-chmod and its deferred restore so
	// unsafe-link mutations always see a writable parent.
	for _, e := range entries {
		if cerr := normalizeExtractChild(ctx, rt, root, p, e, policy, counts); cerr != nil {
			return cerr
		}
	}
	if !isRoot {
		counts.Dirs++
	}
	return nil
}

// readDirSortedIn reads a root-confined directory sorted like os.ReadDir while
// retaining ReadDir's DT_UNKNOWN resolution.
func readDirSortedIn(rt *os.Root, rel string) ([]fs.DirEntry, error) {
	f, err := rt.Open(rel)
	if err != nil {
		return nil, err
	}
	entries, err := f.ReadDir(-1)
	_ = f.Close()
	if err != nil {
		return nil, err
	}
	slices.SortFunc(entries, func(a, b fs.DirEntry) int {
		return strings.Compare(a.Name(), b.Name())
	})
	return entries, nil
}

// classifyExtractSymlink returns an unsafe verdict and raw target without
// following the link; Readlink failures are path-free.
func classifyExtractSymlink(rt *os.Root, root, p string) (unsafe bool, target string, err error) {
	rel, relErr := filepath.Rel(root, p)
	if relErr != nil {
		return false, "", metaErr(relErr)
	}
	target, rerr := rt.Readlink(rel)
	if rerr != nil {
		return false, "", metaErr(rerr)
	}
	return classifySymlinkTarget(root, filepath.Dir(p), target), target, nil
}

// classifySymlinkTarget reports whether an empty, NUL-bearing, absolute, or
// lexically escaping target could alias outside the extracted tree. It performs
// no filesystem access.
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

// applyUnsafeSymlinkPolicy keeps unknown policies fail-safe and applies skip or
// placeholder mutations. Mutation errors are stripped of paths.
func applyUnsafeSymlinkPolicy(rt *os.Root, root, p, target string, policy unsafeSymlinkPolicy) error {
	rel, relErr := filepath.Rel(root, p)
	if relErr != nil {
		return metaErr(relErr)
	}
	switch policy {
	case unsafeSymlinkSkip:
		if err := rt.Remove(rel); err != nil {
			return metaErr(err)
		}
	case unsafeSymlinkPlaceholder:
		if err := rt.Remove(rel); err != nil {
			return metaErr(err)
		}
		// Preserve the target as inert text. os.Root prevents the remove-write gap
		// from escaping staging, though a recreated symlink can redirect within it.
		if err := rt.WriteFile(rel, []byte(target+"\n"), 0o600); err != nil {
			return metaErr(err)
		}
	default:
		// Keep empty or unknown policies verbatim.
	}
	return nil
}

// metaErr wraps a path-free cause as ErrExtractMetadataNormalization.
func metaErr(err error) error {
	return fmt.Errorf("%w: %w", ErrExtractMetadataNormalization, pathFreeCause(err))
}
