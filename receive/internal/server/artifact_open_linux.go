//go:build linux

package server

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"
)

var errArtifactPathChanged = errors.New("artifact path changed during validation")

const (
	// openat2 is syscall 437 on the Linux architectures supported by Codex
	// Remote (amd64 and arm64). The syscall is intentionally used directly so
	// the receiver remains a dependency-free static binary.
	sysOpenat2 = 437

	linuxOPath = 0x200000

	resolveNoMagicLinks = 0x02
	resolveNoSymlinks   = 0x04
	resolveBeneath      = 0x08
)

type openHow struct {
	Flags   uint64
	Mode    uint64
	Resolve uint64
}

// openArtifactFile opens an already-authorized canonical path without ever
// following a symlink during the actual open. CheckTarget resolves legitimate
// symlinks before this function is called; RESOLVE_NO_SYMLINKS then makes a
// component swap between validation and open fail closed instead of escaping
// the configured root. The returned descriptor pins the object used by all
// subsequent type, size and content checks.
func openArtifactFile(canonical string) (*os.File, error) {
	clean := filepath.Clean(canonical)
	if !filepath.IsAbs(clean) {
		return nil, fmt.Errorf("%w: path is not absolute", errArtifactPathChanged)
	}
	relative := strings.TrimPrefix(clean, string(filepath.Separator))
	if relative == "" {
		relative = "."
	}

	rootFD, err := syscall.Open(
		string(filepath.Separator),
		linuxOPath|syscall.O_DIRECTORY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: string(filepath.Separator), Err: err}
	}
	defer syscall.Close(rootFD)

	pathPointer, err := syscall.BytePtrFromString(relative)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid path", errArtifactPathChanged)
	}
	how := openHow{
		Flags:   uint64(syscall.O_RDONLY | syscall.O_CLOEXEC | syscall.O_NOFOLLOW | syscall.O_NONBLOCK),
		Resolve: resolveBeneath | resolveNoMagicLinks | resolveNoSymlinks,
	}
	fd, _, errno := syscall.Syscall6(
		sysOpenat2,
		uintptr(rootFD),
		uintptr(unsafe.Pointer(pathPointer)),
		uintptr(unsafe.Pointer(&how)),
		unsafe.Sizeof(how),
		0,
		0,
	)
	runtime.KeepAlive(pathPointer)
	runtime.KeepAlive(&how)
	if errno != 0 {
		if errno == syscall.ELOOP || errno == syscall.EXDEV {
			return nil, fmt.Errorf("%w: %v", errArtifactPathChanged, errno)
		}
		return nil, &os.PathError{Op: "openat2", Path: clean, Err: errno}
	}

	file := os.NewFile(fd, clean)
	if file == nil {
		_ = syscall.Close(int(fd))
		return nil, errors.New("openat2 returned an invalid descriptor")
	}
	return file, nil
}
