// Package access decides which files a sprig may touch. A sprig's file
// access is its own folder (characters/<id>/) plus paths its owner granted
// explicitly. Thicket's own data (the database, keys, other sprigs'
// folders) is protected: no grant can reach it.
//
// Every path is resolved before it is checked: made absolute, with ".."
// and every symlink along the way followed the way the operating system
// would, so "notes/../../other" and a link pointing out of the folder are
// both seen for what they are.
package access

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ErrOutside means a path is outside what a sprig may use.
var ErrOutside = errors.New("outside this sprig's files")

// maxLinks bounds symlink following, like the kernel's ELOOP limit.
const maxLinks = 40

// Resolve returns the real absolute path p refers to, relative to base
// when p is relative. Components that exist are resolved through their
// symlinks; components that don't exist yet (a file about to be written)
// can't be links, so they are taken as written.
func Resolve(base, p string) (string, error) {
	if strings.ContainsRune(p, 0) {
		return "", errors.New("path contains a NUL byte")
	}
	if p == "" {
		p = "."
	}
	if !filepath.IsAbs(p) {
		if base == "" {
			return "", errors.New("relative path with no folder to start from")
		}
		// Not filepath.Join: it would clean ".." lexically, before the
		// walk below can see whether the component before it is a link.
		p = base + string(filepath.Separator) + p
	}
	// Start from the root and walk component by component.
	vol := filepath.VolumeName(p)
	rest := splitPath(p[len(vol):])
	cur := vol + string(filepath.Separator)
	links := 0
	for len(rest) > 0 {
		c := rest[0]
		rest = rest[1:]
		switch c {
		case "", ".":
			continue
		case "..":
			cur = filepath.Dir(cur) // cur has no links in it, so its parent is real
			continue
		}
		next := filepath.Join(cur, c)
		fi, err := os.Lstat(next)
		if errors.Is(err, fs.ErrNotExist) {
			cur = next
			continue
		}
		if err != nil {
			return "", err
		}
		if fi.Mode()&fs.ModeSymlink == 0 {
			cur = next
			continue
		}
		if links++; links > maxLinks {
			return "", fmt.Errorf("too many symbolic links in %s", p)
		}
		target, err := os.Readlink(next)
		if err != nil {
			return "", err
		}
		if filepath.IsAbs(target) {
			tv := filepath.VolumeName(target)
			cur = tv + string(filepath.Separator)
			target = target[len(tv):]
		}
		rest = append(splitPath(target), rest...)
	}
	return cur, nil
}

func splitPath(p string) []string {
	return strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == filepath.Separator })
}

// Within reports whether path is dir or inside it. Both must already be
// resolved.
func Within(dir, path string) bool {
	if dir == path {
		return true
	}
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// Overlaps reports whether either path contains the other.
func Overlaps(a, b string) bool { return Within(a, b) || Within(b, a) }

// Policy is one sprig's file access.
type Policy struct {
	// Folder is the sprig's own folder: relative paths start here.
	Folder string
	// Grants are the extra paths its owner allowed.
	Grants []string
	// Protected paths are never reachable, granted or not, except where
	// they hold Folder itself (Thicket's data folder holds every sprig's
	// folder).
	Protected []string
}

// Check resolves path and returns the real path if the sprig may use it.
func (p Policy) Check(path string) (string, error) {
	folder, err := Resolve("", p.Folder)
	if err != nil {
		return "", err
	}
	r, err := Resolve(folder, path)
	if err != nil {
		return "", err
	}
	if Within(folder, r) {
		return r, nil
	}
	for _, prot := range p.Protected {
		pr, err := Resolve("", prot)
		if err != nil || Within(pr, r) {
			return "", fmt.Errorf("%s is %w", path, ErrOutside)
		}
	}
	for _, g := range p.Grants {
		gr, err := Resolve("", g)
		if err == nil && Within(gr, r) {
			return r, nil
		}
	}
	return "", fmt.Errorf("%s is %w", path, ErrOutside)
}

// CheckGrant resolves a path an owner wants to grant and refuses one that
// would reach protected data: anything inside, or containing, a protected
// path ("/" contains everything).
func CheckGrant(path string, protected []string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("give the full path, starting with /")
	}
	r, err := Resolve("", path)
	if err != nil {
		return "", err
	}
	for _, prot := range protected {
		pr, err := Resolve("", prot)
		if err != nil {
			return "", err
		}
		if Overlaps(pr, r) {
			return "", fmt.Errorf("%s would reach Thicket's own data (%s), which no sprig can be given", path, prot)
		}
	}
	return r, nil
}
