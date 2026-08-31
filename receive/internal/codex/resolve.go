package codex

import (
	"debug/elf"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

type resolvedBinary struct {
	Path     string
	ExtraEnv []string
	Native   bool
}

func resolveCodexBinary(requested string) (resolvedBinary, error) {
	if requested == "" {
		requested = "auto"
	}
	lookup := requested
	if requested == "auto" {
		lookup = "codex"
	}
	path, err := exec.LookPath(lookup)
	if err != nil {
		return resolvedBinary{}, err
	}
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		realPath = path
	}
	if isELF(realPath) {
		return resolvedBinary{Path: realPath, Native: true}, nil
	}
	if requested != "auto" {
		return resolvedBinary{Path: path, Native: false}, nil
	}

	root := filepath.Dir(filepath.Dir(realPath))
	triple, packageName, ok := nativeCodexTarget()
	if ok {
		candidates := []string{
			filepath.Join(root, "node_modules", "@openai", packageName, "vendor", triple, "bin", "codex"),
			filepath.Join(root, "vendor", triple, "bin", "codex"),
		}
		for _, candidate := range candidates {
			if isExecutable(candidate) && isELF(candidate) {
				return resolvedBinary{
					Path: candidate,
					ExtraEnv: []string{
						"CODEX_MANAGED_PACKAGE_ROOT=" + root,
						"CODEX_MANAGED_BY_NPM=1",
					},
					Native: true,
				}, nil
			}
		}
	}
	return resolvedBinary{Path: path, Native: false}, nil
}

func nativeCodexTarget() (triple, packageName string, ok bool) {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "linux/amd64":
		return "x86_64-unknown-linux-musl", "codex-linux-x64", true
	case "linux/arm64":
		return "aarch64-unknown-linux-musl", "codex-linux-arm64", true
	case "darwin/amd64":
		return "x86_64-apple-darwin", "codex-darwin-x64", true
	case "darwin/arm64":
		return "aarch64-apple-darwin", "codex-darwin-arm64", true
	default:
		return "", "", false
	}
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}

func isELF(path string) bool {
	file, err := elf.Open(path)
	if err != nil {
		return false
	}
	_ = file.Close()
	return true
}

var errNoDaemon = errors.New("Codex shared daemon is unavailable")

func daemonSocketPath(codexHome string) (string, error) {
	if codexHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		codexHome = filepath.Join(home, ".codex")
	}
	return filepath.Join(codexHome, "app-server-control", "app-server-control.sock"), nil
}
