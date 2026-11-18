package dashboard

import (
	"embed"
	"io/fs"
	"net/http"
)

// pageFiles is the built page: web/ builds it into page/, and the build is
// committed, so building hataori needs no Node (design, section 7.2). CI
// builds it again and fails when the result differs from what is committed.
//
//go:embed all:page
var pageFiles embed.FS

// contentSecurityPolicy is sent with every response. The page loads
// nothing but its own server's files: no CDN, no web fonts, no inline
// script, and no other site may frame it.
const contentSecurityPolicy = "default-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'; object-src 'none'"

// page is the built page's files, index.html at the root.
func page() fs.FS {
	sub, err := fs.Sub(pageFiles, "page")
	if err != nil {
		panic(err) // "page" is a valid path; fs.Sub fails only on an invalid one
	}
	return sub
}

// pageRoutes serves the page: index.html at "/", its files under /assets/.
// Like every route, they answer only a signed-in session.
func (s *Server) pageRoutes() {
	files := page()
	s.mux.Handle("GET /assets/", http.FileServerFS(files))
	s.mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, files, "index.html")
	})
}
