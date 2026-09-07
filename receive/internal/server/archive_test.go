package server

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestArchiveMixedSelectionAndProjectBoundary(t *testing.T) {
	s, b, password := newTestServerInstance(t, false)
	root := b.insideCWD
	os.MkdirAll(filepath.Join(root, "folder", "empty"), 0700)
	os.WriteFile(filepath.Join(root, "folder", "nested.txt"), []byte("nested"), 0600)
	os.WriteFile(filepath.Join(root, "single.txt"), []byte("single"), 0600)
	cookie := loginCookie(t, s.Handler(), password)
	send := func(paths ...string) *httptest.ResponseRecorder {
		form := url.Values{"threadId": {"inside"}, "path": paths}
		r := httptest.NewRequest(http.MethodPost, "/api/files/archive", bytes.NewBufferString(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", "http://example.com")
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	response := send(filepath.Join(root, "folder"), filepath.Join(root, "single.txt"), filepath.Join(root, "folder", "nested.txt"))
	if response.Code != 200 {
		t.Fatalf("%d %s", response.Code, response.Body.String())
	}
	archive, err := zip.NewReader(bytes.NewReader(response.Body.Bytes()), int64(response.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, f := range archive.File {
		reader, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(reader)
		reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := files[f.Name]; ok {
			t.Fatal("duplicate ZIP entry")
		}
		files[f.Name] = string(data)
	}
	if !reflect.DeepEqual(files, map[string]string{"folder/": "", "folder/empty/": "", "folder/nested.txt": "nested", "single.txt": "single"}) {
		t.Fatal(files)
	}
	if send("../outside").Code != 400 {
		t.Fatal("project escape allowed")
	}
	os.Symlink(t.TempDir(), filepath.Join(root, "external"))
	if send("external").Code != 400 {
		t.Fatal("external symlink allowed")
	}
}

func TestHTMLPreviewHasIsolatedPolicy(t *testing.T) {
	s, b, password := newTestServerInstance(t, false)
	os.WriteFile(filepath.Join(b.insideCWD, "page.html"), []byte("<style>h1{color:red}</style><h1>预览</h1>"), 0600)
	cookie := loginCookie(t, s.Handler(), password)
	response := doRequest(s.Handler(), http.MethodGet, "/api/files?threadId=inside&path=page.html&preview=1", "", cookie, "")
	if response.Code != 200 || response.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatal(response.Body.String())
	}
	if response.Header().Get("Content-Security-Policy") != "sandbox; default-src 'none'; style-src 'unsafe-inline'; img-src data:; base-uri 'none'; form-action 'none'; frame-ancestors 'self'" {
		t.Fatal("unsafe HTML policy")
	}
	source := doRequest(s.Handler(), http.MethodGet, "/api/files?threadId=inside&path=page.html", "", cookie, "")
	if source.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatal("source mode changed")
	}
}

func TestArchiveDocumentPolicyAndSourceRejection(t *testing.T) {
	s, _, password := newTestServerInstance(t, false)
	document := doRequest(s.Handler(), http.MethodGet, "/", "", nil, "")
	if policy := document.Header().Get("Referrer-Policy"); policy != "same-origin" {
		t.Fatalf("POST navigation Origin would be suppressed by %q", policy)
	}
	cookie := loginCookie(t, s.Handler(), password)
	for _, tc := range []struct{ origin, site string }{{"null", "same-origin"}, {"https://evil.test", "cross-site"}} {
		request := httptest.NewRequest(http.MethodPost, "http://receiver.test/api/files/archive", nil)
		request.AddCookie(cookie)
		request.Header.Set("Origin", tc.origin)
		request.Header.Set("Sec-Fetch-Site", tc.site)
		response := httptest.NewRecorder()
		s.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Fatalf("untrusted Origin %q accepted: %d", tc.origin, response.Code)
		}
	}
}
