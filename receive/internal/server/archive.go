package server

import (
	"archive/zip"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func (s *Server) handleArchive(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := r.ParseForm(); err != nil {
		writeError(w, 400, "invalid_selection", "无法读取文件选择")
		return
	}
	workspace, err := s.threadWorkspace(r.Context(), r.Form.Get("threadId"))
	if err != nil {
		writeError(w, 400, "invalid_workspace", "无法读取会话项目")
		return
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		writeError(w, 400, "invalid_workspace", "无法读取项目目录")
		return
	}
	defer root.Close()
	paths := r.Form["path"]
	if len(paths) == 0 || len(paths) > maxProjectEntries {
		writeError(w, 400, "invalid_selection", "请选择 1 至 2000 个项目文件或目录")
		return
	}
	var names []string
	seen := map[string]bool{}
	var total int64
	var visit func(string) error
	visit = func(name string) error {
		if seen[name] {
			return nil
		}
		seen[name] = true
		if len(seen) > 20000 {
			return errors.New("打包最多包含 20000 项，请缩小选择范围")
		}
		if err := r.Context().Err(); err != nil {
			return err
		}
		f, err := root.Open(name)
		if err != nil {
			return errors.New("所选文件无法读取或链接指向项目外部")
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if info.IsDir() {
			names = append(names, name+"/")
			children, err := f.ReadDir(20001 - len(seen))
			f.Close()
			if err != nil && err != io.EOF {
				return err
			}
			if len(children)+len(seen) > 20000 {
				return errors.New("打包最多包含 20000 项，请缩小选择范围")
			}
			sort.Slice(children, func(i, j int) bool { return children[i].Name() < children[j].Name() })
			for _, child := range children {
				// Do not follow symlink directories recursively (cycles / external targets).
				if child.Type()&os.ModeSymlink != 0 {
					return errors.New("目录含符号链接，请改为选择实际文件或目录")
				}
				if err := visit(filepath.Join(name, child.Name())); err != nil {
					return err
				}
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			return errors.New("打包仅支持常规文件和目录")
		}
		total += info.Size()
		if total > 2<<30 {
			return errors.New("打包原始文件总量超过 2 GiB，请分批下载")
		}
		names = append(names, name)
		return nil
	}
	for _, name := range paths {
		if filepath.IsAbs(name) {
			name, err = filepath.Rel(workspace, name)
		} else {
			name = filepath.Clean(name)
		}
		if err != nil || !filepath.IsLocal(name) {
			err = errors.New("只能打包当前会话项目内的文件和目录")
			break
		}
		if err = visit(name); err != nil {
			break
		}
	}
	if err != nil {
		writeError(w, 400, "archive_unavailable", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filepath.Base(workspace) + "-files.zip"}))
	writer := zip.NewWriter(w)
	for _, name := range names {
		if strings.HasSuffix(name, "/") {
			_, err = writer.CreateHeader(&zip.FileHeader{Name: filepath.ToSlash(name), Method: zip.Store})
		} else {
			var f *os.File
			f, err = root.Open(name)
			if err == nil {
				var info os.FileInfo
				info, err = f.Stat()
				if err == nil && !info.Mode().IsRegular() {
					err = errors.New("file changed during archive")
				}
				if err == nil {
					var header *zip.FileHeader
					header, err = zip.FileInfoHeader(info)
					if err == nil {
						header.Name = filepath.ToSlash(name)
						header.Method = zip.Deflate
						var out io.Writer
						out, err = writer.CreateHeader(header)
						if err == nil {
							_, err = io.CopyN(out, f, info.Size())
						}
					}
				}
				f.Close()
			}
		}
		if err != nil {
			s.cfg.Logger.Printf("archive interrupted: %v", err)
			panic(http.ErrAbortHandler)
		}
	}
	if err = writer.Close(); err != nil {
		panic(http.ErrAbortHandler)
	}
}
