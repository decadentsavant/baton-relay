package main

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"sort"
	"strings"
	"time"
)

//go:embed web/*
var webFiles embed.FS

func webAssets() fs.FS {
	assets, err := fs.Sub(webFiles, "web")
	if err != nil {
		panic(err)
	}
	return assets
}

var pageTemplate = template.Must(template.ParseFS(webFiles, "web/index.html"))

type publicBaton struct {
	ID        string
	Hops      int64
	Age       string
	Countries int
}

func countLabel(n int64, singular, plural string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, singular)
	}
	return fmt.Sprintf("%d %s", n, plural)
}

func (b publicBaton) HopLabel() string { return countLabel(b.Hops, "hop", "hops") }
func (b publicBaton) CountryLabel() string {
	return countLabel(int64(b.Countries), "country", "countries")
}

type pageData struct {
	Title, Description string
	Selected           *publicBaton
	Batons             []publicBaton
	Total              int64
	Online             int
}

// TotalLabel groups digits so a large wave count reads at a glance.
func (d pageData) TotalLabel() string { return commas(d.Total) }

func commas(n int64) string {
	digits := fmt.Sprint(n)
	if n < 0 {
		return "-" + commas(-n)
	}
	var out strings.Builder
	for i, c := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			out.WriteByte(',')
		}
		out.WriteRune(c)
	}
	return out.String()
}

func publicRow(b *baton, now time.Time) publicBaton {
	return publicBaton{b.ID, b.Hops, ageLabel(now.Sub(b.Born)), len(b.Countries)}
}

// ageLabel mirrors Model.ageLabel in the widget, so a baton reads the same on
// the public page as in the tooltip. Deliberately coarse: a precise age plus a
// hop count would narrow down when a specific person was online.
func ageLabel(age time.Duration) string {
	mins := int64(age / time.Minute)
	switch {
	case mins < 1:
		return "just now"
	case mins < 60:
		return countLabel(mins, "minute", "minutes")
	case mins < 24*60:
		return countLabel(mins/60, "hour", "hours")
	}
	days := mins / (24 * 60)
	switch {
	case days < 14:
		return countLabel(days, "day", "days")
	case days < 63:
		return fmt.Sprintf("%d weeks", days/7)
	}
	return countLabel(days/30, "month", "months")
}

func (r *relay) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /wave", r.handleWave)
	mux.HandleFunc("GET /stream", r.handleStream)
	mux.HandleFunc("GET /batons", r.handleBatons)
	mux.HandleFunc("GET /stats", r.handleStats)
	mux.HandleFunc("GET /healthz", r.handleHealth)
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServerFS(webAssets())))
	mux.HandleFunc("GET /map", func(w http.ResponseWriter, req *http.Request) { http.Redirect(w, req, "./", http.StatusSeeOther) })
	mux.HandleFunc("GET /", r.handlePage)
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		mux.ServeHTTP(w, req)
	})
}

func (r *relay) handlePage(w http.ResponseWriter, req *http.Request) {
	id := ""
	if req.URL.Path != "/" {
		if !strings.HasPrefix(req.URL.Path, "/b/") {
			http.NotFound(w, req)
			return
		}
		id = strings.TrimPrefix(req.URL.Path, "/b/")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(w, req)
			return
		}
	}
	data := pageData{Title: "Baton — a small hello, somewhere in the world", Description: "Wave at a random Omarchy user. One bit out, one bit back."}
	r.mu.Lock()
	data.Total, data.Online = r.total, len(r.clients)
	now := time.Now()
	if id != "" {
		if b := r.batons[id]; b != nil {
			row := publicRow(b, now)
			data.Selected = &row
			data.Title = fmt.Sprintf("Baton · %s · %s", row.HopLabel(), row.CountryLabel())
			data.Description = fmt.Sprintf("%s · alive %s · %s. Put a wave in your Omarchy bar.", row.HopLabel(), row.Age, row.CountryLabel())
		}
	} else {
		for _, b := range r.batons {
			data.Batons = append(data.Batons, publicRow(b, now))
		}
	}
	r.mu.Unlock()
	if id != "" && data.Selected == nil {
		http.NotFound(w, req)
		return
	}
	sort.Slice(data.Batons, func(i, j int) bool {
		if data.Batons[i].Hops == data.Batons[j].Hops {
			return data.Batons[i].ID < data.Batons[j].ID
		}
		return data.Batons[i].Hops > data.Batons[j].Hops
	})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
	if err := pageTemplate.Execute(w, data); err != nil {
		return
	}
}
