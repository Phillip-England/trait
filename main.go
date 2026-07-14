package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/yuin/goldmark"
)

const (
	sessionCookie = "trait_session"
	failWindow    = 24 * time.Hour
	maxFailures   = 5
	sessionTTL    = 12 * time.Hour
)

type Config struct {
	AdminUsername string
	AdminPassword string
	SessionSecret string
	DBPath        string
	TraitsDir     string
	LogoPath      string
	Addr          string
	AccentColor   string
}

type App struct {
	cfg      Config
	db       *sql.DB
	sessions *SessionStore
	md       goldmark.Markdown
}

type SessionStore struct {
	mu       sync.Mutex
	sessions map[string]time.Time
}

type Trait struct {
	Slug    string
	Title   string
	Content string
	HTML    template.HTML
	Tags    []string
	ModTime time.Time
}

type PageData struct {
	Config     Config
	Traits     []Trait
	Trait      Trait
	Tags       []string
	ActiveTag  string
	Search     string
	Error      string
	IsAuthed   bool
	AdminRoute bool
	BodyClass  string
}

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "init":
		if err := runInit(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
	case "serve":
		if err := runServe(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Println("usage:")
	fmt.Println("  trait init -env /path/to/trait.env")
	fmt.Println("  trait serve -env /path/to/trait.env")
}

func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	envPath := fs.String("env", ".env", "path to write the env file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	secret, err := randomToken(32)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(absOrClean(*envPath)), 0o755); err != nil {
		return err
	}
	content := fmt.Sprintf(`ADMIN_USERNAME=admin
ADMIN_PASSWORD=change-me-now
SESSION_SECRET=%s
DB_PATH=trait.sqlite
TRAITS_DIR=traits
ADDR=:8080
ACCENT_COLOR=#35d07f
`, secret)
	if _, err := os.Stat(*envPath); err == nil {
		return fmt.Errorf("%s already exists", *envPath)
	}
	if err := os.WriteFile(*envPath, []byte(content), 0o600); err != nil {
		return err
	}
	fmt.Printf("created %s\n", *envPath)
	return nil
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	envPath := fs.String("env", "", "required path to env file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *envPath == "" {
		return errors.New("serve requires -env /path/to/trait.env")
	}

	cfg, err := loadConfig(*envPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.TraitsDir, 0o755); err != nil {
		return err
	}
	db, err := sql.Open("sqlite3", cfg.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := initDB(db); err != nil {
		return err
	}

	app := &App{
		cfg:      cfg,
		db:       db,
		sessions: &SessionStore{sessions: map[string]time.Time{}},
		md:       goldmark.New(),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/controls", app.controls)
	mux.HandleFunc("/", app.publicIndex)
	mux.HandleFunc("/traits/", app.publicTrait)
	mux.HandleFunc("/login", app.login)
	mux.HandleFunc("/logout", app.logout)
	mux.HandleFunc("/admin", app.requireAuth(app.adminIndex))
	mux.HandleFunc("/admin/new", app.requireAuth(app.adminNew))
	mux.HandleFunc("/admin/edit/", app.requireAuth(app.adminEdit))
	mux.HandleFunc("/admin/delete/", app.requireAuth(app.adminDelete))
	mux.HandleFunc("/assets/logo.png", app.logo)
	mux.HandleFunc("/assets/logo-nav.png", app.navLogo)
	mux.HandleFunc("/assets/style.css", app.styles)

	log.Printf("trait listening on %s", cfg.Addr)
	return http.ListenAndServe(cfg.Addr, securityHeaders(mux))
}

func loadConfig(envPath string) (Config, error) {
	absEnv, err := filepath.Abs(envPath)
	if err != nil {
		return Config{}, err
	}
	values, err := parseEnv(absEnv)
	if err != nil {
		return Config{}, err
	}
	base := filepath.Dir(absEnv)
	cfg := Config{
		AdminUsername: values["ADMIN_USERNAME"],
		AdminPassword: values["ADMIN_PASSWORD"],
		SessionSecret: values["SESSION_SECRET"],
		DBPath:        values["DB_PATH"],
		TraitsDir:     values["TRAITS_DIR"],
		Addr:          values["ADDR"],
		AccentColor:   values["ACCENT_COLOR"],
	}
	if cfg.Addr == "" {
		cfg.Addr = ":8080"
	}
	if cfg.AccentColor == "" {
		cfg.AccentColor = "#35d07f"
	}
	required := map[string]string{
		"ADMIN_USERNAME": cfg.AdminUsername,
		"ADMIN_PASSWORD": cfg.AdminPassword,
		"SESSION_SECRET": cfg.SessionSecret,
		"DB_PATH":        cfg.DBPath,
		"TRAITS_DIR":     cfg.TraitsDir,
	}
	for key, value := range required {
		if strings.TrimSpace(value) == "" {
			return Config{}, fmt.Errorf("%s is required in %s", key, envPath)
		}
	}
	cfg.DBPath = resolveRelative(base, cfg.DBPath)
	cfg.TraitsDir = resolveRelative(base, cfg.TraitsDir)
	cfg.LogoPath = filepath.Join(base, "logo.png")
	return cfg, nil
}

func parseEnv(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	values := map[string]string{}
	for lineNo, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: expected KEY=value", path, lineNo+1)
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		values[key] = value
	}
	return values, nil
}

func initDB(db *sql.DB) error {
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS login_failures (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  ip TEXT NOT NULL,
  attempted_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_login_failures_ip_time
ON login_failures (ip, attempted_at);
`)
	return err
}

func (a *App) publicIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	traits, tags, err := a.loadTraits()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	active := strings.TrimSpace(r.URL.Query().Get("tag"))
	if active != "" {
		traits = filterTraits(traits, active)
	}
	search := strings.TrimSpace(r.URL.Query().Get("q"))
	if search != "" {
		traits = searchTraits(traits, search)
	}
	var selected Trait
	if len(traits) > 0 {
		selected = traits[0]
	}
	render(w, "public", PageData{Config: a.cfg, Traits: traits, Trait: selected, Tags: tags, ActiveTag: active, Search: search, IsAuthed: a.isAuthed(r), BodyClass: "browser-page"})
}

func (a *App) publicTrait(w http.ResponseWriter, r *http.Request) {
	slug := strings.Trim(strings.TrimPrefix(r.URL.Path, "/traits/"), "/")
	trait, err := a.loadTrait(slug)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	traits, tags, err := a.loadTraits()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	active := strings.TrimSpace(r.URL.Query().Get("tag"))
	if active != "" {
		traits = filterTraits(traits, active)
	}
	search := strings.TrimSpace(r.URL.Query().Get("q"))
	if search != "" {
		traits = searchTraits(traits, search)
	}
	render(w, "trait", PageData{Config: a.cfg, Traits: traits, Trait: trait, Tags: tags, ActiveTag: active, Search: search, IsAuthed: a.isAuthed(r), BodyClass: "browser-page"})
}

func (a *App) controls(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/controls" {
		http.NotFound(w, r)
		return
	}
	render(w, "controls", PageData{Config: a.cfg, IsAuthed: a.isAuthed(r)})
}

func (a *App) adminIndex(w http.ResponseWriter, r *http.Request) {
	traits, tags, err := a.loadTraits()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	render(w, "admin", PageData{Config: a.cfg, Traits: traits, Tags: tags, IsAuthed: true, AdminRoute: true})
}

func (a *App) adminNew(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		render(w, "edit", PageData{Config: a.cfg, Trait: Trait{}, IsAuthed: true, AdminRoute: true})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	title := strings.TrimSpace(r.FormValue("title"))
	content := strings.TrimSpace(r.FormValue("content"))
	if title == "" || content == "" {
		render(w, "edit", PageData{Config: a.cfg, Trait: Trait{Title: title, Content: content}, Error: "Title and markdown are required.", IsAuthed: true, AdminRoute: true})
		return
	}
	slug := uniqueSlug(a.cfg.TraitsDir, slugify(title))
	if !strings.HasPrefix(content, "# ") {
		content = "# " + title + "\n\n" + content
	}
	if err := os.WriteFile(filepath.Join(a.cfg.TraitsDir, slug+".md"), []byte(content+"\n"), 0o644); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (a *App) adminEdit(w http.ResponseWriter, r *http.Request) {
	slug := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/edit/"), "/")
	if !validSlug(slug) {
		http.NotFound(w, r)
		return
	}
	trait, err := a.loadTrait(slug)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodGet {
		render(w, "edit", PageData{Config: a.cfg, Trait: trait, IsAuthed: true, AdminRoute: true})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	content := strings.TrimSpace(r.FormValue("content"))
	if content == "" {
		trait.Content = content
		render(w, "edit", PageData{Config: a.cfg, Trait: trait, Error: "Markdown is required.", IsAuthed: true, AdminRoute: true})
		return
	}
	if err := os.WriteFile(filepath.Join(a.cfg.TraitsDir, slug+".md"), []byte(content+"\n"), 0o644); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (a *App) adminDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	slug := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/delete/"), "/")
	if !validSlug(slug) {
		http.NotFound(w, r)
		return
	}
	if err := os.Remove(filepath.Join(a.cfg.TraitsDir, slug+".md")); err != nil && !errors.Is(err, os.ErrNotExist) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (a *App) login(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		render(w, "login", PageData{Config: a.cfg})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ip := clientIP(r)
	blocked, err := a.isBlocked(ip)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if blocked {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	user := r.FormValue("username")
	pass := r.FormValue("password")
	if subtleEqual(user, a.cfg.AdminUsername) && subtleEqual(pass, a.cfg.AdminPassword) {
		if err := a.createSession(w); err != nil {
			http.Error(w, "could not create session", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	blocked, err = a.recordFailure(ip)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if blocked {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	render(w, "login", PageData{Config: a.cfg, Error: "Invalid username or password."})
}

func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		if id, ok := a.verifyCookie(cookie.Value); ok {
			a.sessions.Delete(id)
		}
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *App) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.isAuthed(r) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

func (a *App) isAuthed(r *http.Request) bool {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	id, ok := a.verifyCookie(cookie.Value)
	return ok && a.sessions.Valid(id)
}

func (a *App) createSession(w http.ResponseWriter) error {
	id, err := randomToken(32)
	if err != nil {
		return err
	}
	a.sessions.Put(id, time.Now().Add(sessionTTL))
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    a.signCookie(id),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(sessionTTL),
	})
	return nil
}

func (a *App) signCookie(id string) string {
	mac := hmac.New(sha256.New, []byte(a.cfg.SessionSecret))
	mac.Write([]byte(id))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return id + "." + sig
}

func (a *App) verifyCookie(value string) (string, bool) {
	id, sig, ok := strings.Cut(value, ".")
	if !ok || id == "" || sig == "" {
		return "", false
	}
	return id, hmac.Equal([]byte(a.signCookie(id)), []byte(value))
}

func (s *SessionStore) Put(id string, expires time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[id] = expires
}

func (s *SessionStore) Valid(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	expires, ok := s.sessions[id]
	if !ok || time.Now().After(expires) {
		delete(s.sessions, id)
		return false
	}
	return true
}

func (s *SessionStore) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
}

func (a *App) isBlocked(ip string) (bool, error) {
	if err := a.purgeFailures(); err != nil {
		return false, err
	}
	count, err := a.failureCount(ip)
	return count >= maxFailures, err
}

func (a *App) recordFailure(ip string) (bool, error) {
	if err := a.purgeFailures(); err != nil {
		return false, err
	}
	if _, err := a.db.Exec(`INSERT INTO login_failures (ip, attempted_at) VALUES (?, ?)`, ip, time.Now().Unix()); err != nil {
		return false, err
	}
	count, err := a.failureCount(ip)
	return count >= maxFailures, err
}

func (a *App) purgeFailures() error {
	_, err := a.db.Exec(`DELETE FROM login_failures WHERE attempted_at < ?`, time.Now().Add(-failWindow).Unix())
	return err
}

func (a *App) failureCount(ip string) (int, error) {
	var count int
	err := a.db.QueryRow(`SELECT COUNT(*) FROM login_failures WHERE ip = ? AND attempted_at >= ?`, ip, time.Now().Add(-failWindow).Unix()).Scan(&count)
	return count, err
}

func (a *App) loadTraits() ([]Trait, []string, error) {
	var traits []Trait
	err := filepath.WalkDir(a.cfg.TraitsDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(path) != ".md" {
			return nil
		}
		trait, err := a.readTrait(path)
		if err != nil {
			return err
		}
		traits = append(traits, trait)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(traits, func(i, j int) bool {
		return strings.ToLower(traits[i].Title) < strings.ToLower(traits[j].Title)
	})
	tagSet := map[string]bool{}
	for _, trait := range traits {
		for _, tag := range trait.Tags {
			tagSet[tag] = true
		}
	}
	tags := make([]string, 0, len(tagSet))
	for tag := range tagSet {
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	return traits, tags, nil
}

func (a *App) loadTrait(slug string) (Trait, error) {
	if !validSlug(slug) {
		return Trait{}, os.ErrNotExist
	}
	return a.readTrait(filepath.Join(a.cfg.TraitsDir, slug+".md"))
}

func (a *App) readTrait(path string) (Trait, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Trait{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return Trait{}, err
	}
	content := string(raw)
	title := titleFromMarkdown(content)
	if title == "" {
		title = strings.TrimSuffix(filepath.Base(path), ".md")
	}
	var buf bytes.Buffer
	if err := a.md.Convert(raw, &buf); err != nil {
		return Trait{}, err
	}
	return Trait{
		Slug:    strings.TrimSuffix(filepath.Base(path), ".md"),
		Title:   title,
		Content: content,
		HTML:    template.HTML(buf.String()),
		Tags:    tagsFromMarkdown(content),
		ModTime: info.ModTime(),
	}, nil
}

var tagRe = regexp.MustCompile(`(^|[\s(])#([A-Za-z0-9][A-Za-z0-9_-]*)`)

func tagsFromMarkdown(content string) []string {
	matches := tagRe.FindAllStringSubmatch(content, -1)
	seen := map[string]bool{}
	for _, match := range matches {
		seen[strings.ToLower(match[2])] = true
	}
	tags := make([]string, 0, len(seen))
	for tag := range seen {
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	return tags
}

func titleFromMarkdown(content string) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "# "))
		}
	}
	return ""
}

func filterTraits(traits []Trait, tag string) []Trait {
	var out []Trait
	for _, trait := range traits {
		for _, candidate := range trait.Tags {
			if candidate == tag {
				out = append(out, trait)
				break
			}
		}
	}
	return out
}

func searchTraits(traits []Trait, query string) []Trait {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return traits
	}
	var out []Trait
	for _, trait := range traits {
		haystack := strings.ToLower(trait.Title + " " + strings.Join(trait.Tags, " ") + " " + trait.Content)
		if strings.Contains(haystack, query) {
			out = append(out, trait)
		}
	}
	return out
}

func render(w http.ResponseWriter, name string, data PageData) {
	tpl := template.Must(template.New("base").Funcs(template.FuncMap{
		"joinTags": func(tags []string) string {
			if len(tags) == 0 {
				return "untagged"
			}
			parts := make([]string, len(tags))
			for i, tag := range tags {
				parts[i] = "#" + tag
			}
			return strings.Join(parts, " ")
		},
		"traitURL": func(slug, tag, search string) string {
			values := url.Values{}
			if tag != "" {
				values.Set("tag", tag)
			}
			if search != "" {
				values.Set("q", search)
			}
			if encoded := values.Encode(); encoded != "" {
				return "/traits/" + slug + "?" + encoded
			}
			return "/traits/" + slug
		},
		"libraryURL": func(tag, search string) string {
			values := url.Values{}
			if tag != "" {
				values.Set("tag", tag)
			}
			if search != "" {
				values.Set("q", search)
			}
			if encoded := values.Encode(); encoded != "" {
				return "/?" + encoded
			}
			return "/"
		},
	}).Parse(templates))
	if err := tpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (a *App) styles(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	_, _ = w.Write([]byte(strings.ReplaceAll(css, "__ACCENT__", a.cfg.AccentColor)))
}

func (a *App) logo(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, a.cfg.LogoPath)
}

func (a *App) navLogo(w http.ResponseWriter, r *http.Request) {
	path := filepath.Join(filepath.Dir(a.cfg.LogoPath), "logo-nav.png")
	if _, err := os.Stat(path); err != nil {
		path = a.cfg.LogoPath
	}
	http.ServeFile(w, r, path)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func randomToken(bytesLen int) (string, error) {
	buf := make([]byte, bytesLen)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func subtleEqual(a, b string) bool {
	return hmac.Equal([]byte(a), []byte(b))
}

func resolveRelative(base, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(base, path)
}

func absOrClean(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return abs
}

func slugify(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		return "trait"
	}
	return slug
}

func validSlug(value string) bool {
	return value != "" && value == slugify(value)
}

func uniqueSlug(dir, base string) string {
	slug := base
	for i := 2; ; i++ {
		if _, err := os.Stat(filepath.Join(dir, slug+".md")); errors.Is(err, os.ErrNotExist) {
			return slug
		}
		slug = fmt.Sprintf("%s-%d", base, i)
	}
}

const templates = `
{{define "top"}}<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>trait</title>
  <link rel="icon" type="image/png" href="/assets/logo.png">
  <link rel="stylesheet" href="/assets/style.css">
</head>
<body class="{{.BodyClass}}">
  <header class="topbar">
    <a class="brand" href="/">
      <img src="/assets/logo-nav.png" alt="trait" width="180" height="54">
    </a>
    {{if .IsAuthed}}
    <button class="menu-toggle" type="button" aria-label="Open navigation" aria-expanded="false" aria-controls="site-menu" data-menu-open>
      <span></span><span></span><span></span>
    </button>
    {{end}}
  </header>
  {{if .IsAuthed}}
  <div class="menu-overlay" data-menu-close></div>
  <nav class="side-menu" id="site-menu" aria-label="Primary navigation">
    <div class="side-menu-head">
      <span>Navigation</span>
      <button class="menu-close" type="button" aria-label="Close navigation" data-menu-close>&times;</button>
    </div>
    <a href="/">Public</a>
    <a href="/admin">Admin</a>
    <a href="/logout">Logout</a>
  </nav>
  {{end}}
  <main>{{end}}

{{define "bottom"}}</main>
<script>
(function () {
  var menu = document.querySelector("[data-menu-open]");
  var closeTargets = Array.prototype.slice.call(document.querySelectorAll("[data-menu-close]"));

  if (menu) {
    menu.addEventListener("click", openMenu);
    closeTargets.forEach(function (target) {
      target.addEventListener("click", closeMenu);
    });
    document.addEventListener("keydown", function (event) {
      if (event.key === "Escape") closeMenu();
    });
  }

  function openMenu() {
    document.body.classList.add("nav-open");
    menu.setAttribute("aria-expanded", "true");
  }

  function closeMenu() {
    document.body.classList.remove("nav-open");
    if (menu) menu.setAttribute("aria-expanded", "false");
  }

  var storageKey = "trait.cart.v1";
  var cart = loadCart();
  var cards = Array.prototype.slice.call(document.querySelectorAll("[data-trait]"));
  var countEl = document.querySelector("[data-cart-count]");
  var checkoutBtn = document.querySelector("[data-cart-checkout]");
  var clearBtn = document.querySelector("[data-cart-clear]");
  var statusEl = document.querySelector("[data-cart-status]");
  var searchInput = document.querySelector("[data-trait-search]");
  var emptyEl = document.querySelector("[data-search-empty]");

  cards.forEach(function (card) {
    var item = traitFromElement(card);
    if (item.slug && cart[item.slug]) {
      cart[item.slug] = Object.assign({}, cart[item.slug] || {}, item);
    }
    var checkbox = card.querySelector("[data-cart-toggle]");
    if (!checkbox) return;
    checkbox.checked = Boolean(cart[item.slug]);
    checkbox.addEventListener("change", function () {
      if (checkbox.checked) {
        cart[item.slug] = item;
      } else {
        delete cart[item.slug];
      }
      persist();
      renderCart();
    });
  });

  Array.prototype.slice.call(document.querySelectorAll("[data-copy-trait]")).forEach(function (button) {
    button.addEventListener("click", function () {
      var holder = button.closest("[data-trait]");
      var item = holder ? traitFromElement(holder) : null;
      if (!item || !item.content) {
        setStatus("Nothing to copy.");
        return;
      }
      writeClipboard(item.content.trim() + "\n").then(function () {
        setStatus("Copied " + (item.title || "trait") + ".");
      }).catch(function () {
        setStatus("Clipboard failed. Try again from the browser.");
      });
    });
  });

  if (checkoutBtn) {
    checkoutBtn.addEventListener("click", function () {
      var items = cartItems();
      if (!items.length) {
        setStatus("Select traits first.");
        return;
      }
      writeClipboard(formatTraits(items)).then(function () {
        setStatus(items.length + " trait" + (items.length === 1 ? "" : "s") + " copied.");
      }).catch(function () {
        setStatus("Clipboard failed. Try again from the browser.");
      });
    });
  }

  if (clearBtn) {
    clearBtn.addEventListener("click", function () {
      cart = {};
      cards.forEach(function (card) {
        var checkbox = card.querySelector("[data-cart-toggle]");
        if (checkbox) checkbox.checked = false;
      });
      persist();
      renderCart();
      setStatus("Cart cleared.");
    });
  }

  if (searchInput) {
    searchInput.addEventListener("input", filterVisibleTraits);
    filterVisibleTraits();
  }

  document.addEventListener("keydown", handleShortcuts);

  persist();
  renderCart();

  function handleShortcuts(event) {
    if (event.defaultPrevented || event.metaKey || event.ctrlKey || event.altKey) return;
    if (isTypingTarget(event.target)) return;

    var key = event.key.length === 1 ? event.key.toLowerCase() : event.key;
    if (key === "f" || key === "/") {
      if (!searchInput) return;
      event.preventDefault();
      searchInput.focus();
      searchInput.select();
      return;
    }
    if (key === "ArrowDown" || key === "ArrowRight") {
      event.preventDefault();
      goToRelativeTrait(1);
      return;
    }
    if (key === "ArrowUp" || key === "ArrowLeft") {
      event.preventDefault();
      goToRelativeTrait(-1);
      return;
    }
    if (key === " " || key === "Spacebar") {
      event.preventDefault();
      toggleCurrentTrait();
      return;
    }
    if (key === "Enter") {
      event.preventDefault();
      openCurrentTrait();
      return;
    }
    if (key === "c") {
      event.preventDefault();
      copyCurrentTrait();
      return;
    }
    if (key === "x") {
      event.preventDefault();
      if (checkoutBtn && !checkoutBtn.disabled) checkoutBtn.click();
      return;
    }
    if (key === "?") {
      event.preventDefault();
      window.location.href = "/controls";
      return;
    }
    if ((key === "b" || key === "t" || key === "Escape") && window.location.pathname === "/controls") {
      event.preventDefault();
      window.location.href = "/";
    }
  }

  function filterVisibleTraits() {
    var query = (searchInput.value || "").trim().toLowerCase();
    var visible = 0;
    cards.forEach(function (card) {
      if (!card.hasAttribute("data-search")) return;
      var matched = !query || (card.getAttribute("data-search-text") || "").toLowerCase().indexOf(query) !== -1;
      card.hidden = !matched;
      if (matched) visible++;
    });
    if (emptyEl) emptyEl.hidden = visible !== 0;
  }

  function isTypingTarget(target) {
    if (!target) return false;
    var tag = (target.tagName || "").toLowerCase();
    return tag === "input" || tag === "textarea" || tag === "select" || target.isContentEditable;
  }

  function visibleListItems() {
    return cards.filter(function (card) {
      return card.hasAttribute("data-search") && !card.hidden;
    });
  }

  function currentListIndex(items) {
    var active = document.querySelector(".trait-list-item.active");
    var index = items.indexOf(active);
    return index >= 0 ? index : 0;
  }

  function goToRelativeTrait(offset) {
    var items = visibleListItems();
    if (!items.length) return;
    var next = currentListIndex(items) + offset;
    if (next < 0) next = items.length - 1;
    if (next >= items.length) next = 0;
    var link = items[next].querySelector(".trait-link");
    if (link) window.location.href = link.href;
  }

  function openCurrentTrait() {
    var active = document.querySelector(".trait-list-item.active") || visibleListItems()[0];
    var link = active ? active.querySelector(".trait-link") : null;
    if (link) window.location.href = link.href;
  }

  function currentTraitElement() {
    var detail = document.querySelector(".reader-panel[data-slug]:not([data-slug=''])");
    if (detail) return detail;
    return document.querySelector(".trait-list-item.active") || visibleListItems()[0] || null;
  }

  function toggleCurrentTrait() {
    var current = currentTraitElement();
    if (!current) return;
    var item = traitFromElement(current);
    if (!item.slug) return;
    if (cart[item.slug]) {
      delete cart[item.slug];
      setStatus("Removed " + (item.title || "trait") + ".");
    } else {
      cart[item.slug] = item;
      setStatus("Added " + (item.title || "trait") + ".");
    }
    persist();
    renderCart();
  }

  function copyCurrentTrait() {
    var current = currentTraitElement();
    var item = current ? traitFromElement(current) : null;
    if (!item || !item.content) {
      setStatus("Nothing to copy.");
      return;
    }
    writeClipboard(item.content.trim() + "\n").then(function () {
      setStatus("Copied " + (item.title || "trait") + ".");
    }).catch(function () {
      setStatus("Clipboard failed. Try again from the browser.");
    });
  }

  function traitFromElement(el) {
    return {
      slug: el.getAttribute("data-slug") || "",
      title: el.getAttribute("data-title") || "",
      content: el.getAttribute("data-content") || ""
    };
  }

  function loadCart() {
    try {
      var parsed = JSON.parse(localStorage.getItem(storageKey) || "{}");
      return parsed && typeof parsed === "object" ? parsed : {};
    } catch (_) {
      return {};
    }
  }

  function persist() {
    try {
      localStorage.setItem(storageKey, JSON.stringify(cart));
    } catch (_) {}
  }

  function cartItems() {
    return Object.keys(cart).sort(function (a, b) {
      return (cart[a].title || a).localeCompare(cart[b].title || b);
    }).map(function (slug) {
      return cart[slug];
    }).filter(function (item) {
      return item && item.content;
    });
  }

  function renderCart() {
    var count = cartItems().length;
    if (countEl) countEl.textContent = String(count);
    if (checkoutBtn) checkoutBtn.disabled = count === 0;
    cards.forEach(function (card) {
      var checkbox = card.querySelector("[data-cart-toggle]");
      if (checkbox) checkbox.checked = Boolean(cart[card.getAttribute("data-slug") || ""]);
    });
    document.body.classList.toggle("has-cart", count > 0);
  }

  function setStatus(message) {
    if (!statusEl) return;
    statusEl.textContent = message;
    window.clearTimeout(setStatus.timer);
    setStatus.timer = window.setTimeout(function () {
      statusEl.textContent = "";
    }, 2600);
  }

  function formatTraits(items) {
    return items.map(function (item) {
      return item.content.trim();
    }).join("\n\n---\n\n") + "\n";
  }

  function writeClipboard(value) {
    if (navigator.clipboard && window.isSecureContext) {
      return navigator.clipboard.writeText(value);
    }
    var textarea = document.createElement("textarea");
    textarea.value = value;
    textarea.setAttribute("readonly", "");
    textarea.style.position = "fixed";
    textarea.style.top = "-999px";
    document.body.appendChild(textarea);
    textarea.select();
    var ok = document.execCommand("copy");
    textarea.remove();
    return ok ? Promise.resolve() : Promise.reject(new Error("copy failed"));
  }
})();
</script>
</body>
</html>{{end}}

{{define "tags"}}
  <div class="tags">
    <a class="tag {{if eq .ActiveTag ""}}active{{end}}" href="/">all</a>
    {{range .Tags}}<a class="tag {{if eq $.ActiveTag .}}active{{end}}" href="/?tag={{.}}">#{{.}}</a>{{end}}
  </div>
{{end}}

{{define "browser"}}
  <section class="browser-shell">
    <div class="browser-toolbar">
      <div>
        <h1>Traits</h1>
        <p>Search, read, select, and copy reusable application traits.</p>
      </div>
      <a class="controls-link" href="/controls">Controls</a>
    </div>
    <section class="trait-browser">
    <aside class="library-panel" aria-label="Trait library">
      <form class="search" method="get" action="/">
        {{if .ActiveTag}}<input type="hidden" name="tag" value="{{.ActiveTag}}">{{end}}
        <label>Search traits<input data-trait-search name="q" value="{{.Search}}" type="search" placeholder="Search title, tag, or markdown" autocomplete="off"></label>
        <button class="button" type="submit">Search</button>
      </form>
      {{template "tags" .}}
      <div class="trait-list">
        {{range .Traits}}
          <article class="trait-list-item {{if eq $.Trait.Slug .Slug}}active{{end}}" data-trait data-search data-slug="{{.Slug}}" data-title="{{.Title}}" data-content="{{.Content}}" data-search-text="{{.Title}} {{joinTags .Tags}} {{.Content}}">
            <label class="pick">
              <input data-cart-toggle type="checkbox" aria-label="Add {{.Title}} to cart">
              <span>Add</span>
            </label>
            <a class="trait-link" href="{{traitURL .Slug $.ActiveTag $.Search}}">
              <span class="meta">{{joinTags .Tags}}</span>
              <strong>{{.Title}}</strong>
            </a>
            <button class="icon-button" type="button" data-copy-trait aria-label="Copy {{.Title}}">Copy</button>
          </article>
		{{end}}
		{{if .Traits}}<p class="empty" data-search-empty hidden>No traits match that search.</p>{{end}}
		{{if not .Traits}}
			{{if .Search}}<p class="empty">No traits match that search.</p>{{else}}<p class="empty">No traits found.</p>{{end}}
		{{end}}
	</div>
    </aside>
    <article class="doc reader-panel" data-trait data-slug="{{.Trait.Slug}}" data-title="{{.Trait.Title}}" data-content="{{.Trait.Content}}">
      {{if .Trait.Slug}}
        <div class="reader-actions">
          <a class="back-link" href="{{libraryURL .ActiveTag .Search}}">Back to library</a>
          <div>
            <label class="pick detail-pick">
              <input data-cart-toggle type="checkbox" aria-label="Add {{.Trait.Title}} to cart">
              <span>Add</span>
            </label>
            <button class="button secondary" type="button" data-copy-trait>Copy trait</button>
          </div>
        </div>
        <div class="meta">{{joinTags .Trait.Tags}}</div>
        {{.Trait.HTML}}
      {{else}}
        <p class="empty">Select a trait to read it.</p>
      {{end}}
    </article>
    </section>
  </section>
  <aside class="cart-bar" aria-live="polite">
    <div><strong data-cart-count>0</strong> selected</div>
    <p data-cart-status></p>
    <button class="secondary" type="button" data-cart-clear>Clear</button>
    <button class="button" type="button" data-cart-checkout disabled>Copy selected</button>
  </aside>
{{end}}

{{define "public"}}{{template "top" .}}
  {{template "browser" .}}
{{template "bottom" .}}{{end}}

{{define "trait"}}{{template "top" .}}
  {{template "browser" .}}
{{template "bottom" .}}{{end}}

{{define "controls"}}{{template "top" .}}
  <section class="section-head">
    <div>
      <h1>Controls</h1>
      <p>Keyboard controls for moving through the trait library quickly.</p>
    </div>
    <a class="button secondary" href="/">Back to library</a>
  </section>
  <section class="controls-grid" aria-label="Keyboard controls">
    <article>
      <kbd>F</kbd>
      <div><strong>Search</strong><span>Focuses and selects the search field.</span></div>
    </article>
    <article>
      <kbd>/</kbd>
      <div><strong>Search</strong><span>Alternate search shortcut.</span></div>
    </article>
    <article>
      <kbd>↓</kbd>
      <div><strong>Next trait</strong><span>Moves to the next visible trait in the list.</span></div>
    </article>
    <article>
      <kbd>↑</kbd>
      <div><strong>Previous trait</strong><span>Moves to the previous visible trait in the list.</span></div>
    </article>
    <article>
      <kbd>→</kbd>
      <div><strong>Next trait</strong><span>Alternate next shortcut.</span></div>
    </article>
    <article>
      <kbd>←</kbd>
      <div><strong>Previous trait</strong><span>Alternate previous shortcut.</span></div>
    </article>
    <article>
      <kbd>Enter</kbd>
      <div><strong>Open trait</strong><span>Opens the currently highlighted trait.</span></div>
    </article>
    <article>
      <kbd>Space</kbd>
      <div><strong>Add current</strong><span>Adds or removes the current trait from the selected set.</span></div>
    </article>
    <article>
      <kbd>C</kbd>
      <div><strong>Copy current</strong><span>Copies the current trait markdown.</span></div>
    </article>
    <article>
      <kbd>X</kbd>
      <div><strong>Copy selected</strong><span>Copies all selected traits when the cart has items.</span></div>
    </article>
    <article>
      <kbd>?</kbd>
      <div><strong>Controls</strong><span>Opens this controls page.</span></div>
    </article>
    <article>
      <kbd>B</kbd>
      <div><strong>Back to traits</strong><span>Returns from the controls page to the trait library.</span></div>
    </article>
    <article>
      <kbd>T</kbd>
      <div><strong>Traits</strong><span>Alternate shortcut for returning to the trait library.</span></div>
    </article>
    <article>
      <kbd>Esc</kbd>
      <div><strong>Back to traits</strong><span>Also returns from the controls page to the trait library.</span></div>
    </article>
  </section>
{{template "bottom" .}}{{end}}

{{define "admin"}}{{template "top" .}}
  <section class="section-head">
    <div>
      <h1>Admin</h1>
      <p>Traits are markdown files. Add tags anywhere as #auth, #db, #ui, or any label you need.</p>
    </div>
    <a class="button" href="/admin/new">New trait</a>
  </section>
  {{template "tags" .}}
  <section class="list">
    {{range .Traits}}
      <article class="row">
        <div>
          <div class="meta">{{joinTags .Tags}}</div>
          <h2><a href="/traits/{{.Slug}}">{{.Title}}</a></h2>
        </div>
        <div class="actions">
          <a class="button secondary" href="/admin/edit/{{.Slug}}">Edit</a>
          <form method="post" action="/admin/delete/{{.Slug}}"><button class="danger" type="submit">Delete</button></form>
        </div>
      </article>
    {{else}}
      <p class="empty">No traits yet.</p>
    {{end}}
  </section>
{{template "bottom" .}}{{end}}

{{define "edit"}}{{template "top" .}}
  <section class="section-head"><h1>{{if .Trait.Slug}}Edit trait{{else}}New trait{{end}}</h1></section>
  {{if .Error}}<p class="error">{{.Error}}</p>{{end}}
  <form class="editor" method="post">
    {{if not .Trait.Slug}}<label>Title<input name="title" value="{{.Trait.Title}}" autocomplete="off"></label>{{end}}
    <label>Markdown<textarea name="content" rows="24">{{.Trait.Content}}</textarea></label>
    <button class="button" type="submit">Save</button>
  </form>
{{template "bottom" .}}{{end}}

{{define "login"}}{{template "top" .}}
  <form class="login" method="post" action="/login">
    <img class="login-logo" src="/assets/logo.png" alt="" width="96" height="64">
    <h1>Admin login</h1>
    {{if .Error}}<p class="error">{{.Error}}</p>{{end}}
    <label>Username<input name="username" autocomplete="username"></label>
    <label>Password<input name="password" type="password" autocomplete="current-password"></label>
    <button class="button" type="submit">Login</button>
  </form>
{{template "bottom" .}}{{end}}
`

const css = `
:root { --accent: __ACCENT__; color-scheme: dark; }
* { box-sizing: border-box; }
html, body { min-height: 100%; }
body { margin: 0; background: #050505; color: #f5f5f5; font-family: ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; line-height: 1.5; }
body.browser-page { height: 100vh; overflow: hidden; }
body.has-cart main { padding-bottom: 0; }
a { color: inherit; text-decoration: none; }
a:hover { color: var(--accent); }
.topbar { display: flex; justify-content: space-between; align-items: center; min-height: 82px; padding: 10px clamp(18px, 4vw, 48px); border-bottom: 1px solid #202020; position: sticky; top: 0; z-index: 30; background: rgba(5,5,5,.92); backdrop-filter: blur(12px); }
.brand { color: var(--accent); font-weight: 800; font-size: 1.1rem; display: inline-flex; align-items: center; gap: 10px; min-width: 0; }
.brand img { width: 180px; height: 54px; object-fit: contain; display: block; flex: 0 0 180px; }
.menu-toggle, .menu-close { width: 42px; height: 42px; padding: 0; border-radius: 6px; background: #151515; color: #f5f5f5; border: 1px solid #303030; display: inline-grid; place-items: center; }
.menu-toggle span { width: 18px; height: 2px; background: currentColor; display: block; }
.menu-toggle { gap: 4px; }
.menu-close { font-size: 1.65rem; line-height: 1; }
.menu-overlay { position: fixed; inset: 0; width: 100vw; height: 100vh; z-index: 40; background: rgba(3,3,3,.72); opacity: 0; pointer-events: none; transition: opacity .22s ease; backdrop-filter: blur(5px); }
.side-menu { position: fixed; top: 0; right: 0; z-index: 50; width: min(360px, calc(100vw - 28px)); height: 100vh; padding: 18px; background: #0b0b0b; border-left: 1px solid #2a2a2a; box-shadow: -18px 0 60px rgba(0,0,0,.62); transform: translateX(100%); transition: transform .24s ease; display: grid; align-content: start; gap: 8px; color: #e8e8e8; }
.side-menu-head { display: flex; justify-content: space-between; align-items: center; gap: 12px; padding-bottom: 16px; margin-bottom: 8px; border-bottom: 1px solid #202020; color: #bdbdbd; font-weight: 750; }
.side-menu a { border: 1px solid transparent; border-radius: 6px; padding: 12px 10px; font-weight: 750; }
.side-menu a:hover { border-color: #2a2a2a; background: #101010; }
.nav-open { overflow: hidden; }
.nav-open .menu-overlay { opacity: 1; pointer-events: auto; }
.nav-open .side-menu { transform: translateX(0); }
main { width: min(1220px, calc(100% - 28px)); margin: 0 auto; padding: 42px 0 72px; }
.browser-page main { height: calc(100vh - 82px); padding: 14px 0; overflow: hidden; }
.browser-shell { height: 100%; min-height: 0; display: grid; grid-template-rows: auto minmax(0, 1fr); gap: 12px; }
.browser-toolbar { display: flex; align-items: center; justify-content: space-between; gap: 18px; padding-bottom: 12px; border-bottom: 1px solid #1c1c1c; }
.browser-toolbar h1 { margin: 0; font-size: clamp(1.45rem, 2.8vw, 2.35rem); line-height: 1; letter-spacing: 0; }
.browser-toolbar p { color: #bdbdbd; margin: 6px 0 0; max-width: 640px; }
.hero { padding: 24px 0 22px; border-bottom: 1px solid #1c1c1c; margin-bottom: 24px; }
.hero h1, .section-head h1 { font-size: clamp(2rem, 5vw, 4.5rem); line-height: 1; margin: 0 0 14px; letter-spacing: 0; max-width: 850px; }
.hero p, .section-head p { color: #bdbdbd; margin: 0; max-width: 680px; }
.controls-link { display: inline-flex; color: var(--accent); font-weight: 750; white-space: nowrap; }
.tags { display: flex; flex-wrap: wrap; gap: 8px; margin: 10px 0; max-height: 84px; overflow: auto; }
.tag { border: 1px solid #2a2a2a; color: #d7d7d7; padding: 7px 10px; border-radius: 6px; }
.tag.active { border-color: var(--accent); color: var(--accent); }
.grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(240px, 1fr)); gap: 14px; }
.card, .row { border: 1px solid #202020; border-radius: 8px; background: #0b0b0b; }
.card { padding: 18px; min-height: 140px; }
.trait-card { display: grid; grid-template-rows: auto 1fr; gap: 22px; }
.card h2, .row h2 { margin: 8px 0 0; font-size: 1.25rem; line-height: 1.2; }
.meta { color: var(--accent); font-size: .85rem; overflow-wrap: anywhere; }
.trait-browser { min-height: 0; display: grid; grid-template-columns: minmax(300px, 380px) minmax(0, 1fr); gap: 18px; align-items: stretch; }
.library-panel { min-height: 0; overflow: hidden; display: grid; grid-template-rows: auto auto minmax(0, 1fr); padding-right: 4px; }
.search { display: grid; grid-template-columns: 1fr auto; gap: 10px; align-items: end; margin-bottom: 8px; }
.search label { min-width: 0; }
.trait-list { min-height: 0; overflow: auto; display: grid; align-content: start; gap: 10px; padding-right: 4px; }
.trait-list-item { display: grid; grid-template-columns: auto minmax(0, 1fr) auto; gap: 10px; align-items: center; padding: 12px; border: 1px solid #202020; border-radius: 8px; background: #0b0b0b; }
.trait-list-item.active { border-color: var(--accent); background: #0f1411; }
.trait-list-item[hidden] { display: none; }
.trait-link { display: grid; gap: 4px; min-width: 0; }
.trait-link strong { font-size: .98rem; line-height: 1.2; overflow-wrap: anywhere; }
.icon-button { background: #161616; color: #f5f5f5; border: 1px solid #303030; padding: 8px 10px; font-size: .85rem; }
.doc { max-width: none; min-width: 0; }
.reader-panel { min-height: 0; overflow: auto; border-left: 1px solid #202020; padding-left: 22px; padding-right: 6px; }
.reader-actions { display: flex; align-items: center; justify-content: space-between; gap: 16px; margin-bottom: 14px; position: sticky; top: 0; z-index: 2; padding: 0 0 12px; background: #050505; }
.reader-actions > div { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; }
.back-link { color: #bdbdbd; font-weight: 750; }
.doc h1 { font-size: clamp(1.75rem, 3vw, 2.75rem); line-height: 1.05; }
.doc h2 { margin-top: 26px; }
.doc p, .doc li { color: #dddddd; }
.doc pre { overflow: auto; padding: 16px; background: #101010; border: 1px solid #242424; border-radius: 8px; }
.doc code { color: #f2f2f2; }
.section-head { display: flex; align-items: end; justify-content: space-between; gap: 20px; margin-bottom: 24px; }
.controls-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(260px, 1fr)); gap: 12px; }
.controls-grid article { display: grid; grid-template-columns: 54px minmax(0, 1fr); gap: 14px; align-items: center; padding: 14px; border: 1px solid #202020; border-radius: 8px; background: #0b0b0b; }
.controls-grid kbd { width: 54px; min-height: 42px; display: grid; place-items: center; border-radius: 6px; border: 1px solid #333; background: #151515; color: var(--accent); font: 800 1rem ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; }
.controls-grid strong, .controls-grid span { display: block; }
.controls-grid span { color: #bdbdbd; font-size: .92rem; }
.button, button { background: var(--accent); color: #031006; border: 0; border-radius: 6px; padding: 10px 14px; font-weight: 750; cursor: pointer; font: inherit; }
.button:disabled, button:disabled { cursor: not-allowed; opacity: .5; }
.secondary { background: #1a1a1a; color: #f5f5f5; border: 1px solid #303030; }
.danger { background: #ff4d4d; color: #160000; }
.pick { justify-self: start; display: inline-flex; grid-auto-flow: column; align-items: center; gap: 8px; color: #e7e7e7; cursor: pointer; user-select: none; }
.pick input { appearance: none; width: 20px; height: 20px; margin: 0; padding: 0; border-radius: 5px; border: 1px solid #3a3a3a; background: #101010; display: grid; place-items: center; }
.pick input:checked { border-color: var(--accent); background: var(--accent); }
.pick input:checked::after { content: ""; width: 10px; height: 6px; border: solid #031006; border-width: 0 0 2px 2px; transform: rotate(-45deg) translate(1px, -1px); }
.pick span { font-weight: 750; font-size: .9rem; }
.detail-pick { margin: 0; }
.cart-bar { position: fixed; left: 50%; bottom: 18px; z-index: 20; width: min(720px, calc(100% - 36px)); transform: translate(-50%, 120%); opacity: 0; pointer-events: none; transition: transform .18s ease, opacity .18s ease; display: flex; align-items: center; gap: 12px; padding: 12px; border: 1px solid #252525; border-radius: 8px; background: rgba(11,11,11,.96); box-shadow: 0 18px 50px rgba(0,0,0,.48); backdrop-filter: blur(12px); }
.has-cart .cart-bar { transform: translate(-50%, 0); opacity: 1; pointer-events: auto; }
.cart-bar div { color: #e8e8e8; min-width: 92px; }
.cart-bar strong { color: var(--accent); }
.cart-bar p { min-height: 1.5em; flex: 1; margin: 0; color: #bdbdbd; font-size: .9rem; }
.list { display: grid; gap: 12px; }
.row { padding: 16px; display: flex; justify-content: space-between; gap: 16px; align-items: center; }
.actions { display: flex; align-items: center; gap: 10px; }
.actions form { margin: 0; }
.editor, .login { display: grid; gap: 16px; max-width: 820px; }
.login { max-width: 380px; margin: 8vh auto 0; border: 1px solid #222; border-radius: 8px; background: #0b0b0b; padding: 22px; }
.login-logo { width: 96px; height: 64px; object-fit: contain; display: block; margin-bottom: 2px; }
label { display: grid; gap: 7px; color: #d8d8d8; }
input, textarea { width: 100%; background: #101010; color: #fff; border: 1px solid #303030; border-radius: 6px; padding: 11px 12px; font: inherit; }
textarea { resize: vertical; font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; }
.error { color: #ff8f8f; }
.empty { color: #a8a8a8; }
@media (max-width: 680px) {
  .topbar { min-height: 76px; }
  .brand img { width: 150px; height: 44px; flex-basis: 150px; }
  .section-head, .row { align-items: flex-start; flex-direction: column; }
  main { width: min(100% - 28px, 1120px); padding-top: 24px; }
  .browser-page main { height: calc(100vh - 76px); padding: 10px 0; }
  .browser-shell { gap: 10px; }
  .browser-toolbar { align-items: flex-start; }
  .browser-toolbar h1 { font-size: 1.5rem; }
  .browser-toolbar p { display: none; }
  .trait-browser { grid-template-columns: 1fr; grid-template-rows: minmax(190px, 36%) minmax(0, 1fr); gap: 12px; }
  .library-panel { min-height: 0; overflow: hidden; padding-right: 0; }
  .tags { max-height: 42px; flex-wrap: nowrap; overflow-x: auto; overflow-y: hidden; }
  .search { grid-template-columns: 1fr; }
  .reader-panel { border-left: 0; border-top: 1px solid #202020; padding: 12px 0 0; }
  .reader-actions { align-items: flex-start; flex-direction: column; position: static; }
  .trait-list-item { grid-template-columns: auto minmax(0, 1fr); }
  .trait-list-item .icon-button { grid-column: 2; justify-self: start; }
  .actions { width: 100%; justify-content: flex-start; flex-wrap: wrap; }
  .cart-bar { align-items: stretch; flex-wrap: wrap; }
  .cart-bar p { flex-basis: 100%; order: 4; }
}
`
