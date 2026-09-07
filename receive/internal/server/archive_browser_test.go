package server

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Opt-in real-browser check: CR_ARCHIVE_BROWSER=1 go test -run TestArchiveBrowser -v ./internal/server.
// Open the printed URL and click Download. It uses temporary files and test credentials only.
func TestArchiveBrowser(t *testing.T) {
	if os.Getenv("CR_ARCHIVE_BROWSER") != "1" {
		t.Skip("requires a browser")
	}
	s, b, password := newTestServerInstance(t, false)
	os.Mkdir(filepath.Join(b.insideCWD, "folder"), 0700)
	os.WriteFile(filepath.Join(b.insideCWD, "folder", "nested.txt"), []byte("nested"), 0600)
	os.WriteFile(filepath.Join(b.insideCWD, "single.txt"), []byte("single"), 0600)
	source, err := os.ReadFile("../../../send/app.js")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(source), "  function downloadFileSelection() {")
	end := strings.Index(string(source)[start:], "\n  function renderProjectPreview()") + start
	script := `const state={selectedId:"inside",projectFileSelection:new Set(["single.txt","folder"])};` + string(source[start:end]) + `document.querySelector("button").addEventListener("click",downloadFileSelection);`
	os.WriteFile(filepath.Join(s.cfg.WebRoot, "archive-test.js"), []byte(script), 0600)
	os.WriteFile(filepath.Join(s.cfg.WebRoot, "index.html"), []byte(`<!doctype html><meta charset="utf-8"><title>Archive browser regression</title><button>Download file and folder</button><script src="/archive-test.js"></script>`), 0600)
	cookie := loginCookie(t, s.Handler(), password)
	result := make(chan *httptest.ResponseRecorder, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.SetCookie(w, cookie)
		}
		if r.URL.Path != "/api/files/archive" {
			s.Handler().ServeHTTP(w, r)
			return
		}
		recorder := httptest.NewRecorder()
		s.Handler().ServeHTTP(recorder, r)
		t.Logf("browser Origin=%q Sec-Fetch-Site=%q status=%d", r.Header.Get("Origin"), r.Header.Get("Sec-Fetch-Site"), recorder.Code)
		for key, values := range recorder.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(recorder.Code)
		w.Write(recorder.Body.Bytes())
		select {
		case result <- recorder:
		default:
		}
	}))
	defer server.Close()
	t.Logf("ARCHIVE_BROWSER_URL=%s", server.URL)
	select {
	case response := <-result:
		expected := 200
		if value := os.Getenv("CR_ARCHIVE_EXPECT_STATUS"); value != "" {
			expected, _ = strconv.Atoi(value)
		}
		if response.Code != expected {
			t.Fatalf("status=%d: %s", response.Code, response.Body.String())
		}
		if expected != 200 {
			return
		}
		archive, err := zip.NewReader(bytes.NewReader(response.Body.Bytes()), int64(response.Body.Len()))
		if err != nil {
			t.Fatal(err)
		}
		files := map[string]string{}
		for _, file := range archive.File {
			reader, err := file.Open()
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(reader)
			reader.Close()
			if err != nil {
				t.Fatal(err)
			}
			files[file.Name] = string(data)
		}
		if len(files) != 3 || files["single.txt"] != "single" || files["folder/nested.txt"] != "nested" {
			t.Fatal(files)
		}
		t.Log("ZIP verified: single.txt + folder/nested.txt")
	case <-time.After(2 * time.Minute):
		t.Fatal("browser did not submit the download")
	}
}
