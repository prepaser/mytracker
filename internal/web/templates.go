package web

import (
	"embed"
	"encoding/json"
	"html/template"
	"io"
	"log/slog"
	"time"
)

//go:embed templates/*
var tmplFS embed.FS

var tpl *template.Template

func init() {
	tpl = template.Must(template.New("").Funcs(template.FuncMap{
		"fmtTime": func(unix int64) string {
			if unix == 0 {
				return "-"
			}
			return time.Unix(unix, 0).Format("2006-01-02 15:04:05")
		},
		"statusClass": func(s string) string {
			switch s {
			case "ok", "confirmed":
				return "ok"
			case "fail":
				return "fail"
			default:
				return "pending"
			}
		},
		"json": func(v any) template.JS {
			b, err := json.Marshal(v)
			if err != nil {
				return template.JS("[]")
			}
			return template.JS(b)
		},
	}).ParseFS(tmplFS, "templates/*.html"))
}

func page(w io.Writer, name string, data any) {
	if err := tpl.ExecuteTemplate(w, name, data); err != nil {
		slog.Error("template render failed", "template", name)
	}
}
