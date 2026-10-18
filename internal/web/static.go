package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var staticRoot embed.FS

func staticHandler() (http.Handler, error) {
	sub, err := fs.Sub(staticRoot, "static")
	if err != nil {
		return nil, err
	}
	return http.StripPrefix("/static/", http.FileServer(http.FS(sub))), nil
}
