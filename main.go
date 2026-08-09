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
	"io"
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
	sessionCookie  = "trait_session"
	failWindow     = 24 * time.Hour
	maxFailures    = 5
	sessionTTL     = 12 * time.Hour
	defaultEnvPath = "config/.env"
	defaultDBPath  = "data/main.sqlite"
	defaultTraits  = "data/traits"
	defaultAddr    = ":6688"
)

type Config struct {
	AdminUsername string
	AdminPassword string
	SessionSecret string
	DBPath        string
	TraitsDir     string
	LogoPath      string
	AccentColor   string
}

type App struct {
	cfg      Config
	db       *sql.DB
	sessions *SessionStore
	md       goldmark.Markdown
	importMu sync.Mutex
}

type SessionStore struct {
	mu       sync.Mutex
	sessions map[string]time.Time
}

type Trait struct {
	Slug          string
	Title         string
	Content       string
	HTML          template.HTML
	Tags          []string
	ModTime       time.Time
	WorkspaceSlug string
	WorkspaceName string
}

type Workspace struct {
	Slug        string
	Name        string
	Description string
	TraitCount  int
}

type PageData struct {
	Config     Config
	Traits     []Trait
	Trait      Trait
	Tags       []string
	ActiveTag  string
	Search     string
	Error      string
	Notice     string
	Workspace  Workspace
	Workspaces []Workspace
	IsAuthed   bool
	AdminRoute bool
	BodyClass  string
}

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		if err := runServe(nil); err != nil {
			log.Fatal(err)
		}
		return
	}

	switch os.Args[1] {
	case "dump":
		if err := runDump(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
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
	fmt.Println("  trait")
	fmt.Println("  trait dump [directory]")
	fmt.Println("  trait init")
	fmt.Println("  trait serve")
}

func runDump(args []string) error {
	fs := flag.NewFlagSet("dump", flag.ContinueOnError)
	force := fs.Bool("force", false, "overwrite an existing TRAIT.md")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir := "."
	if fs.NArg() > 1 {
		return fmt.Errorf("usage: trait dump [directory]")
	}
	if fs.NArg() == 1 {
		dir = fs.Arg(0)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, "TRAIT.md")
	if !*force {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%s already exists; rerun with -force to replace it", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.WriteFile(path, []byte(traitDumpGuide), 0o644); err != nil {
		return err
	}
	fmt.Printf("created %s\n", path)
	return nil
}

func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	envPath := fs.String("env", defaultEnvPath, "path to write the env file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	secret, err := randomToken(32)
	if err != nil {
		return err
	}
	absEnv := absOrClean(*envPath)
	envDir := filepath.Dir(absEnv)
	if err := os.MkdirAll(envDir, 0o755); err != nil {
		return err
	}
	dbPath := resolveRelative(filepath.Dir(envDir), defaultDBPath)
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(resolveRelative(envDir, "../data/uploads"), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(resolveRelative(envDir, "../"+defaultTraits), 0o755); err != nil {
		return err
	}
	content := fmt.Sprintf(`ADMIN_USERNAME=admin
ADMIN_PASSWORD=change-me-now
SESSION_SECRET=%s
DB_PATH=../data/main.sqlite
TRAITS_DIR=../data/traits
ACCENT_COLOR=#35d07f
`, secret)
	if _, err := os.Stat(*envPath); err == nil {
		return fmt.Errorf("%s already exists", *envPath)
	}
	if err := os.WriteFile(*envPath, []byte(content), 0o600); err != nil {
		return err
	}
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := initDB(db); err != nil {
		return err
	}
	fmt.Printf("created %s\n", *envPath)
	return nil
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	envPath := fs.String("env", defaultEnvPath, "path to env file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfig(*envPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.TraitsDir, 0o755); err != nil {
		return err
	}
	if err := seedTraitsDir(cfg.TraitsDir); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0o755); err != nil {
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
	mux.HandleFunc("/", app.showcase)
	mux.HandleFunc("/traits", app.publicTraits)
	mux.HandleFunc("/traits/", app.publicTrait)
	mux.HandleFunc("/workspaces/", app.publicWorkspace)
	mux.HandleFunc("/login", app.login)
	mux.HandleFunc("/logout", app.logout)
	mux.HandleFunc("/admin", app.requireAuth(app.adminIndex))
	mux.HandleFunc("/admin/new", app.requireAuth(app.adminNew))
	mux.HandleFunc("/admin/import", app.requireAuth(app.adminImport))
	mux.HandleFunc("/admin/workspaces/new", app.requireAuth(app.adminWorkspaceNew))
	mux.HandleFunc("/admin/workspaces/", app.requireAuth(app.adminWorkspace))
	mux.HandleFunc("/admin/edit/", app.requireAuth(app.adminEdit))
	mux.HandleFunc("/admin/delete/", app.requireAuth(app.adminDelete))
	mux.HandleFunc("/assets/logo.png", app.logo)
	mux.HandleFunc("/assets/logo-nav.png", app.navLogo)
	mux.HandleFunc("/assets/style.css", app.styles)

	log.Printf("trait listening on %s", defaultAddr)
	return http.ListenAndServe(defaultAddr, securityHeaders(mux))
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
		AccentColor:   values["ACCENT_COLOR"],
	}
	if cfg.DBPath == "" {
		cfg.DBPath = "../" + defaultDBPath
	}
	if cfg.TraitsDir == "" {
		cfg.TraitsDir = "../" + defaultTraits
	}
	if cfg.AccentColor == "" {
		cfg.AccentColor = "#35d07f"
	}
	required := map[string]string{
		"ADMIN_USERNAME": cfg.AdminUsername,
		"ADMIN_PASSWORD": cfg.AdminPassword,
		"SESSION_SECRET": cfg.SessionSecret,
	}
	for key, value := range required {
		if strings.TrimSpace(value) == "" {
			return Config{}, fmt.Errorf("%s is required in %s", key, envPath)
		}
	}
	cfg.DBPath = resolveRelative(base, cfg.DBPath)
	cfg.TraitsDir = resolveRelative(base, cfg.TraitsDir)
	cfg.LogoPath = resolveAssetPath(base, "logo.png")
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
CREATE TABLE IF NOT EXISTS workspaces (
  slug TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);
`)
	return err
}

func (a *App) showcase(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	traits, tags, err := a.loadTraits()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	render(w, "showcase", PageData{Config: a.cfg, Traits: traits, Trait: featuredTrait(traits), Tags: tags, IsAuthed: a.isAuthed(r), BodyClass: "showcase-page"})
}

func (a *App) publicTraits(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/traits" {
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
	workspaces, _ := a.loadWorkspaces()
	if search != "" {
		workspaces = searchWorkspaces(workspaces, search)
	}
	render(w, "public", PageData{Config: a.cfg, Traits: traits, Trait: selected, Tags: tags, Workspaces: workspaces, ActiveTag: active, Search: search, IsAuthed: a.isAuthed(r), BodyClass: "browser-page"})
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
	render(w, "trait", PageData{Config: a.cfg, Traits: traits, Trait: trait, Tags: tags, ActiveTag: active, Search: search, IsAuthed: a.isAuthed(r), BodyClass: "browser-page detail-page"})
}

func (a *App) publicWorkspace(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/workspaces/"), "/"), "/")
	if len(parts) == 0 || !validSlug(parts[0]) {
		http.NotFound(w, r)
		return
	}
	workspace, err := a.loadWorkspace(parts[0])
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if len(parts) == 3 && parts[1] == "traits" {
		trait, err := a.loadWorkspaceTrait(workspace, parts[2])
		if err != nil {
			http.NotFound(w, r)
			return
		}
		traits, tags, err := a.loadWorkspaceTraits(workspace)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		render(w, "trait", PageData{Config: a.cfg, Workspace: workspace, Traits: traits, Trait: trait, Tags: tags, Search: strings.TrimSpace(r.URL.Query().Get("q")), IsAuthed: a.isAuthed(r), BodyClass: "browser-page detail-page"})
		return
	}
	if len(parts) != 1 {
		http.NotFound(w, r)
		return
	}
	traits, tags, err := a.loadWorkspaceTraits(workspace)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	search := strings.TrimSpace(r.URL.Query().Get("q"))
	if search != "" {
		traits = searchTraits(traits, search)
	}
	var selected Trait
	if len(traits) > 0 {
		selected = traits[0]
	}
	render(w, "public", PageData{Config: a.cfg, Workspace: workspace, Traits: traits, Trait: selected, Tags: tags, Search: search, IsAuthed: a.isAuthed(r), BodyClass: "browser-page"})
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
	workspaces, err := a.loadWorkspaces()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	render(w, "admin", PageData{Config: a.cfg, Traits: traits, Tags: tags, Workspaces: workspaces, Notice: strings.TrimSpace(r.URL.Query().Get("notice")), IsAuthed: true, AdminRoute: true})
}

const (
	maxImportBytes = 64 << 20
	maxTraitBytes  = 2 << 20
)

type uploadedTrait struct {
	title   string
	content []byte
}

func (a *App) adminWorkspaceNew(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		render(w, "workspaceEdit", PageData{Config: a.cfg, IsAuthed: true, AdminRoute: true})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	description := strings.TrimSpace(r.FormValue("description"))
	if name == "" {
		render(w, "workspaceEdit", PageData{Config: a.cfg, Workspace: Workspace{Name: name, Description: description}, Error: "A workspace name is required.", IsAuthed: true, AdminRoute: true})
		return
	}
	base := slugify(name)
	slug := base
	for i := 2; ; i++ {
		_, err := a.db.Exec(`INSERT INTO workspaces (slug, name, description, created_at) VALUES (?, ?, ?, ?)`, slug, name, description, time.Now().Unix())
		if err == nil {
			break
		}
		if !strings.Contains(strings.ToLower(err.Error()), "unique") {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		slug = fmt.Sprintf("%s-%d", base, i)
	}
	if err := os.MkdirAll(a.workspaceDir(slug), 0o755); err != nil {
		_, _ = a.db.Exec(`DELETE FROM workspaces WHERE slug = ?`, slug)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin/workspaces/"+slug, http.StatusSeeOther)
}

func (a *App) adminWorkspace(w http.ResponseWriter, r *http.Request) {
	slug := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/workspaces/"), "/")
	workspace, err := a.loadWorkspace(slug)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	traits, tags, err := a.loadWorkspaceTraits(workspace)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	render(w, "admin", PageData{Config: a.cfg, Workspace: workspace, Traits: traits, Tags: tags, Notice: strings.TrimSpace(r.URL.Query().Get("notice")), IsAuthed: true, AdminRoute: true})
}

// adminImport accepts both an individual Markdown file and the collection of
// files produced by a browser directory picker. Directory structure is
// intentionally flattened because traits are stored as a flat slug library.
func (a *App) adminImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxImportBytes)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		http.Error(w, "Upload must be a Markdown file or a directory of Markdown files (64 MB maximum).", http.StatusBadRequest)
		return
	}
	defer r.MultipartForm.RemoveAll()
	destination := a.cfg.TraitsDir
	workspaceSlug := strings.TrimSpace(r.FormValue("workspace"))
	if workspaceSlug != "" {
		workspace, err := a.loadWorkspace(workspaceSlug)
		if err != nil {
			http.Error(w, "Workspace not found.", http.StatusBadRequest)
			return
		}
		destination = a.workspaceDir(workspace.Slug)
	}

	var pending []uploadedTrait
	skipped := 0
	for _, headers := range r.MultipartForm.File {
		for _, header := range headers {
			if !strings.EqualFold(filepath.Ext(header.Filename), ".md") {
				skipped++
				continue
			}
			file, err := header.Open()
			if err != nil {
				http.Error(w, "Could not read "+filepath.Base(header.Filename)+".", http.StatusBadRequest)
				return
			}
			content, err := io.ReadAll(io.LimitReader(file, maxTraitBytes+1))
			file.Close()
			if err != nil || len(content) > maxTraitBytes {
				http.Error(w, filepath.Base(header.Filename)+" is unreadable or larger than 2 MB.", http.StatusBadRequest)
				return
			}
			content = bytes.TrimSpace(content)
			if len(content) == 0 {
				skipped++
				continue
			}
			title := titleFromMarkdown(string(content))
			if title == "" {
				title = strings.TrimSuffix(filepath.Base(header.Filename), filepath.Ext(header.Filename))
				content = append([]byte("# "+title+"\n\n"), content...)
			}
			pending = append(pending, uploadedTrait{title: title, content: content})
		}
	}
	if len(pending) == 0 {
		http.Error(w, "No non-empty Markdown files were found in that upload.", http.StatusBadRequest)
		return
	}

	// Keep slug selection and writes together so simultaneous uploads cannot
	// select the same collision suffix.
	a.importMu.Lock()
	defer a.importMu.Unlock()
	written := make([]string, 0, len(pending))
	for _, trait := range pending {
		path, err := writeUniqueTrait(destination, slugify(trait.title), append(trait.content, '\n'))
		if err != nil {
			for _, created := range written {
				_ = os.Remove(created)
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		written = append(written, path)
	}
	notice := fmt.Sprintf("Imported %d trait", len(written))
	if len(written) != 1 {
		notice += "s"
	}
	if skipped > 0 {
		notice += fmt.Sprintf("; skipped %d non-Markdown or empty file", skipped)
		if skipped != 1 {
			notice += "s"
		}
	}
	notice += ". Name collisions were resolved automatically."
	redirect := "/admin"
	if workspaceSlug != "" {
		redirect = "/admin/workspaces/" + workspaceSlug
	}
	http.Redirect(w, r, redirect+"?notice="+url.QueryEscape(notice), http.StatusSeeOther)
}

func (a *App) adminNew(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		var workspace Workspace
		if slug := strings.TrimSpace(r.URL.Query().Get("workspace")); slug != "" {
			workspace, _ = a.loadWorkspace(slug)
		}
		render(w, "edit", PageData{Config: a.cfg, Workspace: workspace, Trait: Trait{}, IsAuthed: true, AdminRoute: true})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	title := strings.TrimSpace(r.FormValue("title"))
	content := strings.TrimSpace(r.FormValue("content"))
	workspaceSlug := strings.TrimSpace(r.FormValue("workspace"))
	var workspace Workspace
	if workspaceSlug != "" {
		workspace, _ = a.loadWorkspace(workspaceSlug)
	}
	if title == "" || content == "" {
		render(w, "edit", PageData{Config: a.cfg, Workspace: workspace, Trait: Trait{Title: title, Content: content}, Error: "Title and markdown are required.", IsAuthed: true, AdminRoute: true})
		return
	}
	if !strings.HasPrefix(content, "# ") {
		content = "# " + title + "\n\n" + content
	}
	destination := a.cfg.TraitsDir
	if workspaceSlug != "" {
		if _, err := a.loadWorkspace(workspaceSlug); err != nil {
			http.Error(w, "Workspace not found.", http.StatusBadRequest)
			return
		}
		destination = a.workspaceDir(workspaceSlug)
	}
	if _, err := writeUniqueTrait(destination, slugify(title), []byte(content+"\n")); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if workspaceSlug != "" {
		http.Redirect(w, r, "/admin/workspaces/"+workspaceSlug, http.StatusSeeOther)
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
	workspaceSlug := strings.TrimSpace(r.URL.Query().Get("workspace"))
	directory := a.cfg.TraitsDir
	var workspace Workspace
	var trait Trait
	var err error
	if workspaceSlug != "" {
		workspace, err = a.loadWorkspace(workspaceSlug)
		if err == nil {
			trait, err = a.loadWorkspaceTrait(workspace, slug)
			directory = a.workspaceDir(workspaceSlug)
		}
	} else {
		trait, err = a.loadTrait(slug)
	}
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodGet {
		render(w, "edit", PageData{Config: a.cfg, Workspace: workspace, Trait: trait, IsAuthed: true, AdminRoute: true})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	content := strings.TrimSpace(r.FormValue("content"))
	if content == "" {
		trait.Content = content
		render(w, "edit", PageData{Config: a.cfg, Workspace: workspace, Trait: trait, Error: "Markdown is required.", IsAuthed: true, AdminRoute: true})
		return
	}
	if err := os.WriteFile(filepath.Join(directory, slug+".md"), []byte(content+"\n"), 0o644); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if workspaceSlug != "" {
		http.Redirect(w, r, "/admin/workspaces/"+workspaceSlug, http.StatusSeeOther)
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
	directory := a.cfg.TraitsDir
	workspaceSlug := strings.TrimSpace(r.URL.Query().Get("workspace"))
	if workspaceSlug != "" {
		if _, err := a.loadWorkspace(workspaceSlug); err != nil {
			http.NotFound(w, r)
			return
		}
		directory = a.workspaceDir(workspaceSlug)
	}
	if err := os.Remove(filepath.Join(directory, slug+".md")); err != nil && !errors.Is(err, os.ErrNotExist) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if workspaceSlug != "" {
		http.Redirect(w, r, "/admin/workspaces/"+workspaceSlug, http.StatusSeeOther)
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
	entries, err := os.ReadDir(a.cfg.TraitsDir)
	if err != nil {
		return nil, nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
			continue
		}
		trait, err := a.readTrait(filepath.Join(a.cfg.TraitsDir, entry.Name()))
		if err != nil {
			return nil, nil, err
		}
		traits = append(traits, trait)
	}
	workspaces, err := a.loadWorkspaces()
	if err != nil {
		return nil, nil, err
	}
	for _, workspace := range workspaces {
		workspaceTraits, _, err := a.loadWorkspaceTraits(workspace)
		if err != nil {
			return nil, nil, err
		}
		traits = append(traits, workspaceTraits...)
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

func (a *App) workspaceDir(slug string) string {
	return filepath.Join(a.cfg.TraitsDir, "workspaces", slug)
}

func (a *App) loadWorkspaces() ([]Workspace, error) {
	rows, err := a.db.Query(`SELECT slug, name, description FROM workspaces ORDER BY lower(name)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var workspaces []Workspace
	for rows.Next() {
		var workspace Workspace
		if err := rows.Scan(&workspace.Slug, &workspace.Name, &workspace.Description); err != nil {
			return nil, err
		}
		entries, _ := os.ReadDir(a.workspaceDir(workspace.Slug))
		for _, entry := range entries {
			if !entry.IsDir() && strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
				workspace.TraitCount++
			}
		}
		workspaces = append(workspaces, workspace)
	}
	return workspaces, rows.Err()
}

func (a *App) loadWorkspace(slug string) (Workspace, error) {
	var workspace Workspace
	err := a.db.QueryRow(`SELECT slug, name, description FROM workspaces WHERE slug = ?`, slug).Scan(&workspace.Slug, &workspace.Name, &workspace.Description)
	return workspace, err
}

func (a *App) loadWorkspaceTraits(workspace Workspace) ([]Trait, []string, error) {
	entries, err := os.ReadDir(a.workspaceDir(workspace.Slug))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var traits []Trait
	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
			continue
		}
		trait, err := a.readTraitIn(filepath.Join(a.workspaceDir(workspace.Slug), entry.Name()), workspace)
		if err != nil {
			return nil, nil, err
		}
		traits = append(traits, trait)
	}
	sort.Slice(traits, func(i, j int) bool { return strings.ToLower(traits[i].Title) < strings.ToLower(traits[j].Title) })
	tagSet := map[string]bool{}
	for _, trait := range traits {
		for _, tag := range trait.Tags {
			tagSet[tag] = true
		}
	}
	var tags []string
	for tag := range tagSet {
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	return traits, tags, nil
}

func (a *App) loadWorkspaceTrait(workspace Workspace, slug string) (Trait, error) {
	if !validSlug(slug) {
		return Trait{}, os.ErrNotExist
	}
	return a.readTraitIn(filepath.Join(a.workspaceDir(workspace.Slug), slug+".md"), workspace)
}

func seedTraitsDir(target string) error {
	empty, err := traitsDirIsEmpty(target)
	if err != nil {
		return err
	}
	if !empty {
		return nil
	}
	for _, source := range []string{"traits", "/app/seed-traits"} {
		if samePath(source, target) {
			continue
		}
		if _, err := os.Stat(source); err != nil {
			continue
		}
		return copyMarkdownTree(source, target)
	}
	return nil
}

func traitsDirIsEmpty(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	return len(entries) == 0, nil
}

func copyMarkdownTree(source, target string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		dest := filepath.Join(target, rel)
		if entry.IsDir() {
			return os.MkdirAll(dest, 0o755)
		}
		if filepath.Ext(path) != ".md" {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dest, raw, 0o644)
	})
}

func samePath(a, b string) bool {
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	return errA == nil && errB == nil && absA == absB
}

func (a *App) loadTrait(slug string) (Trait, error) {
	if !validSlug(slug) {
		return Trait{}, os.ErrNotExist
	}
	return a.readTrait(filepath.Join(a.cfg.TraitsDir, slug+".md"))
}

func (a *App) readTrait(path string) (Trait, error) {
	return a.readTraitIn(path, Workspace{})
}

func (a *App) readTraitIn(path string, workspace Workspace) (Trait, error) {
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
		Slug:          strings.TrimSuffix(filepath.Base(path), ".md"),
		Title:         title,
		Content:       content,
		HTML:          template.HTML(buf.String()),
		Tags:          tagsFromMarkdown(content),
		ModTime:       info.ModTime(),
		WorkspaceSlug: workspace.Slug,
		WorkspaceName: workspace.Name,
	}, nil
}

var tagRe = regexp.MustCompile(`#([A-Za-z0-9][A-Za-z0-9_-]*)`)

func tagsFromMarkdown(content string) []string {
	matches := tagRe.FindAllStringSubmatch(content, -1)
	seen := map[string]bool{}
	for _, match := range matches {
		seen[strings.ToLower(match[1])] = true
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

func searchWorkspaces(workspaces []Workspace, query string) []Workspace {
	query = strings.ToLower(strings.TrimSpace(query))
	var out []Workspace
	for _, workspace := range workspaces {
		if strings.Contains(strings.ToLower(workspace.Name+" "+workspace.Description), query) {
			out = append(out, workspace)
		}
	}
	return out
}

func featuredTrait(traits []Trait) Trait {
	for _, trait := range traits {
		if trait.Slug == "application-control-system" {
			return trait
		}
	}
	if len(traits) > 0 {
		return traits[0]
	}
	return Trait{}
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
		"traitPath": func(trait Trait) string {
			if trait.WorkspaceSlug != "" {
				return "/workspaces/" + trait.WorkspaceSlug + "/traits/" + trait.Slug
			}
			return "/traits/" + trait.Slug
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
				return "/traits?" + encoded
			}
			return "/traits"
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

func resolveAssetPath(envDir, name string) string {
	candidates := []string{
		filepath.Join(envDir, name),
		filepath.Join(filepath.Dir(envDir), name),
		filepath.Join(".", name),
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return filepath.Join(filepath.Dir(envDir), name)
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

func writeUniqueTrait(dir, base string, content []byte) (string, error) {
	for i := 1; ; i++ {
		slug := base
		if i > 1 {
			slug = fmt.Sprintf("%s-%d", base, i)
		}
		path := filepath.Join(dir, slug+".md")
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if _, err := file.Write(content); err != nil {
			_ = file.Close()
			_ = os.Remove(path)
			return "", err
		}
		if err := file.Close(); err != nil {
			_ = os.Remove(path)
			return "", err
		}
		return path, nil
	}
}

const traitDumpGuide = `# TRAIT.md

This project uses the trait system to extract reusable implementation patterns from a codebase. A trait is a concise markdown document that describes one repeatable behavior, architecture choice, integration pattern, UI pattern, security control, deployment convention, or developer workflow found in the project.

When an LLM is asked to inspect this project for traits, it should read this file first, inspect the repository, then create a ` + "`./traits`" + ` directory containing one markdown file per trait it finds.

## What Counts As A Trait

A good trait is:

- Specific enough to reuse in another project.
- Grounded in real files, commands, configuration, or behavior found in this repository.
- Written as implementation guidance, not as a changelog or generic documentation.
- Small enough that one trait describes one coherent pattern.
- Useful to a future LLM or engineer trying to reproduce the same pattern elsewhere.

Examples of traits include:

- Persistent SQLite storage under a project-owned data directory.
- Docker and Makefile commands that mount runtime directories.
- Admin authentication with IP-based rate limiting.
- A mobile navigation drawer with accessible controls.
- An explicit environment configuration schema.
- A startup routine that creates required runtime directories.

Do not create traits for one-off details that are not reusable, generated files, vendored dependencies, build artifacts, or secrets.

## Trait File Format

Create traits in ` + "`./traits`" + `. Each trait should be a markdown file named with a lowercase kebab-case slug:

~~~text
traits/persistent-sqlite-data-directory.md
traits/mobile-navigation-drawer.md
traits/docker-runtime-mounts.md
~~~

Each trait must start with YAML front matter. Put reusable hashtags in the ` + "`hashtags`" + ` list. Hashtags must include the leading ` + "`#`" + ` character so an admin portal can ingest them directly.

~~~markdown
---
title: Persistent SQLite Data Directory
hashtags:
  - "#sqlite"
  - "#database"
  - "#persistence"
  - "#docker"
---

# Persistent SQLite Data Directory

## Intent

Describe the reusable pattern in one or two short paragraphs.

## When To Use

Explain the project situations where this trait applies.

## Implementation

Describe the concrete implementation. Include relevant paths, commands, environment variables, schema fields, handlers, components, or configuration names.

## Project Evidence

Reference the files or directories that prove this trait exists in the project.

## Reuse Notes

Explain what another project should copy, adapt, or avoid.
~~~

Keep the first ` + "`# Heading`" + ` aligned with the front matter title. Use additional markdown hashtags in the body only when they are useful; the canonical tags live in front matter.

## Inspection Workflow For An LLM

1. Read ` + "`TRAIT.md`" + ` completely.
2. Inspect the project tree with a fast file search.
3. Read the main application entry points, configuration files, Docker files, package manifests, database setup, UI components, and tests.
4. Identify repeated or reusable implementation patterns.
5. Create ` + "`./traits`" + ` if it does not exist.
6. Write one markdown file per discovered trait using the format above.
7. Keep every trait grounded in project evidence.
8. Do not include secrets, credentials, generated databases, local runtime files, dependency directories, or private machine paths.

## Quality Bar

Each trait should teach a future implementation. Prefer concrete details over broad claims:

- Use exact file and directory names when they matter.
- Name environment variables and commands.
- Describe startup behavior and failure modes.
- Mention security boundaries and persistence boundaries.
- Include enough detail for another LLM to reproduce the pattern without rereading the whole project.

Avoid vague traits such as "uses Go" or "has a website" unless the project contains a distinctive reusable pattern around that technology.
`

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
  <div class="asset-loading" role="status" aria-live="polite">Loading...</div>
  <header class="topbar site-header">
    <div class="site-header-inner page-container">
      <a class="brand site-logo" href="/">
        <img src="/assets/logo-nav.png" alt="trait" width="180" height="54">
      </a>
      <nav class="topnav site-nav" aria-label="Primary navigation">
        <a href="/traits">Browse traits</a>
        {{if .IsAuthed}}<a href="/admin">Admin</a><a href="/logout">Logout</a>{{else}}<a href="/login">Sign in</a>{{end}}
      </nav>
      <button class="menu-toggle" type="button" aria-label="Open navigation" aria-expanded="false" aria-controls="site-menu" data-menu-open>
        <span></span><span></span><span></span>
      </button>
    </div>
  </header>
  <div class="menu-overlay" data-menu-close></div>
  <nav class="side-menu" id="site-menu" aria-label="Primary navigation">
    <div class="side-menu-head">
      <span>Navigation</span>
      <button class="menu-close" type="button" aria-label="Close navigation" data-menu-close>&times;</button>
    </div>
    <a href="/">Showcase</a>
    <a href="/traits">Traits</a>
    <a href="/admin">Admin</a>
    {{if .IsAuthed}}<a href="/logout">Logout</a>{{else}}<a href="/login">Sign in</a>{{end}}
  </nav>
  <main>{{end}}

{{define "bottom"}}</main>
<script>
(function () {
  document.body.classList.add("assets-ready");

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
  var searchItems = Array.prototype.slice.call(document.querySelectorAll("[data-search]"));

  cards.forEach(function (card) {
    var item = traitFromElement(card);
    if (item.slug && cart[item.slug]) {
      cart[item.slug] = Object.assign({}, cart[item.slug] || {}, item);
    }
    var toggle = card.querySelector("[data-cart-toggle]");
    if (toggle) {
      toggle.addEventListener("click", function () {
        toggleTrait(item);
      });
    }
    if (card.classList.contains("trait-list-item")) {
      card.addEventListener("click", function (event) {
        if (event.target.closest("a, button")) return;
        toggleTrait(item);
      });
    }
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
      window.location.href = "/traits";
    }
  }

  function filterVisibleTraits() {
    var query = (searchInput.value || "").trim().toLowerCase();
    var visible = 0;
    searchItems.forEach(function (card) {
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
    return Array.prototype.slice.call(document.querySelectorAll(".trait-list-item[data-search]")).filter(function (card) {
      return !card.hidden;
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
    toggleTrait(item);
  }

  function toggleTrait(item) {
    if (!item || !item.slug) return;
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
      var selected = Boolean(cart[card.getAttribute("data-slug") || ""]);
      card.classList.toggle("selected", selected);
      card.setAttribute("aria-selected", selected ? "true" : "false");
      var toggle = card.querySelector("[data-cart-toggle]");
      if (toggle) {
        toggle.setAttribute("aria-pressed", selected ? "true" : "false");
        toggle.textContent = selected ? "Selected" : "Select trait";
      }
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
    <a class="tag {{if eq .ActiveTag ""}}active{{end}}" href="/traits">all</a>
    {{range .Tags}}<a class="tag {{if eq $.ActiveTag .}}active{{end}}" href="/traits?tag={{.}}">#{{.}}</a>{{end}}
  </div>
{{end}}

{{define "contentTags"}}
  <div class="content-tags">
    {{range .}}<span class="content-tag">#{{.}}</span>{{else}}<span class="content-tag">untagged</span>{{end}}
  </div>
{{end}}

{{define "showcase"}}{{template "top" .}}
  <section class="showcase-hero hero">
    <div class="showcase-copy hero-copy">
      <p class="eyebrow">Trait library for application patterns</p>
      <h1 class="hero-title">Reusable patterns for better applications.</h1>
      <p class="hero-description">A focused library of reusable implementation patterns for building application infrastructure, UI shells, deployment workflows, and operational guardrails.</p>
      <div class="hero-actions">
        <a class="button" href="/traits">Browse traits</a>
        {{if .Trait.Slug}}<a class="text-link" href="{{traitPath .Trait}}">View featured trait</a>{{end}}
      </div>
    </div>
    <div class="showcase-panel featured-trait-card" aria-label="Featured trait preview">
      <div class="panel-head featured-trait-header">
        <span>Featured trait</span>
        {{if .Trait.Slug}}<a href="{{traitPath .Trait}}">Read trait</a>{{end}}
      </div>
      <div class="featured-trait featured-trait-body">
        {{if .Trait.Slug}}
          {{template "contentTags" .Trait.Tags}}
          <h2 class="featured-trait-title">{{.Trait.Title}}</h2>
          <p>Reusable guidance stored as markdown, ready to read, select, and copy into an implementation brief.</p>
          <div class="featured-meta"><span>Markdown source</span><span>{{len .Trait.Tags}} tags</span></div>
          <a class="button secondary" href="{{traitPath .Trait}}">Open trait</a>
        {{else}}
          <p class="empty">No traits have been published yet.</p>
        {{end}}
      </div>
    </div>
  </section>
  <section class="showcase-band">
    <article>
      <span class="feature-icon" aria-hidden="true">R</span>
      <h2>Reusable patterns</h2>
      <p>Traits describe complete implementation behaviors you can bring into new or existing applications.</p>
    </article>
    <article>
      <span class="feature-icon" aria-hidden="true">F</span>
      <h2>Fast browsing</h2>
      <p>The dedicated trait browser supports search, tags, focused reading, and copying selected traits.</p>
    </article>
    <article>
      <span class="feature-icon" aria-hidden="true">M</span>
      <h2>Markdown source</h2>
      <p>Each trait is plain markdown, so the guidance remains portable, reviewable, and easy to evolve.</p>
    </article>
  </section>
{{template "bottom" .}}{{end}}

{{define "browser"}}
  <section class="browser-shell">
    <div class="browser-toolbar">
      <div>
        <p class="eyebrow">Library</p>
        <h1>Traits</h1>
        <p>Search, read, select, and copy reusable application traits.</p>
      </div>
      <a class="controls-link" href="/controls">Controls</a>
    </div>
    <section class="trait-browser">
    <aside class="library-panel" aria-label="Trait library">
      <div class="sidebar-head">
        <h2>Library</h2>
        <span>{{len .Traits}} results</span>
      </div>
      <form class="search" method="get" action="{{if .Workspace}}/workspaces/{{.Workspace.Slug}}{{else}}/traits{{end}}">
        {{if .ActiveTag}}<input type="hidden" name="tag" value="{{.ActiveTag}}">{{end}}
        <label>Search traits<input data-trait-search name="q" value="{{.Search}}" type="search" placeholder="Search title, tag, or markdown" autocomplete="off"></label>
        <button class="button secondary" type="submit">Search</button>
      </form>
      {{if .Workspace}}
        <div class="workspace-context"><strong>{{.Workspace.Name}}</strong><p>{{.Workspace.Description}}</p><a href="/traits">Search all workspaces</a></div>
      {{else if .Workspaces}}
        <div class="workspace-links"><span>Workspaces</span>{{range .Workspaces}}<a href="/workspaces/{{.Slug}}"><strong>{{.Name}}</strong><small>{{.TraitCount}} traits</small></a>{{end}}</div>
      {{end}}
      <div class="filter-head"><span>Tags</span>{{if or .ActiveTag .Search}}<a href="/traits">Reset</a>{{end}}</div>
      {{template "tags" .}}
    </aside>
    <section class="results-panel" aria-label="Trait results">
      <div class="list-toolbar">
        <span>{{len .Traits}} results</span>
        <div class="bulk-actions">
          <span><strong data-cart-count>0</strong> selected</span>
          <button class="secondary" type="button" data-cart-clear>Clear</button>
          <button class="button" type="button" data-cart-checkout disabled>Copy selected</button>
        </div>
      </div>
      <div class="trait-list">
        {{range .Traits}}
          <article class="trait-list-item {{if eq $.Trait.Slug .Slug}}active{{end}}" data-trait data-search data-slug="{{.Slug}}" data-title="{{.Title}}" data-content="{{.Content}}" data-search-text="{{.Title}} {{joinTags .Tags}} {{.Content}}">
            <a class="trait-link" href="{{traitPath .}}">
              <strong>{{.Title}}</strong>
              {{template "contentTags" .Tags}}
            </a>
            <button class="icon-button" type="button" data-copy-trait aria-label="Copy {{.Title}}" title="Copy trait">Copy</button>
          </article>
		{{end}}
		{{if .Traits}}<p class="empty" data-search-empty hidden>No traits match that search.</p>{{end}}
		{{if not .Traits}}
			{{if .Search}}<p class="empty">No traits match that search.</p>{{else}}<p class="empty">No traits found.</p>{{end}}
		{{end}}
	</div>
    </section>
    <article class="doc reader-panel" data-trait data-slug="{{.Trait.Slug}}" data-title="{{.Trait.Title}}" data-content="{{.Trait.Content}}">
      {{if .Trait.Slug}}
        <header class="reader-head">
          <a class="back-link" href="{{if .Workspace}}/workspaces/{{.Workspace.Slug}}{{else}}{{libraryURL .ActiveTag .Search}}{{end}}">Back to library</a>
          <h1>{{.Trait.Title}}</h1>
          {{template "contentTags" .Trait.Tags}}
          <div>
            <button class="button secondary select-toggle" type="button" data-cart-toggle aria-pressed="false">Select trait</button>
            <button class="button secondary" type="button" data-copy-trait>Copy trait</button>
          </div>
        </header>
        <div class="article-body">{{.Trait.HTML}}</div>
      {{else}}
        <p class="empty">Select a trait to read it.</p>
      {{end}}
    </article>
    </section>
  </section>
  <aside class="cart-bar" aria-live="polite">
    <p data-cart-status></p>
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
    <a class="button secondary" href="/traits">Back to library</a>
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
  <section class="section-head admin-head">
    <div>
      <h1>{{if .Workspace}}{{.Workspace.Name}}{{else}}Trait administration{{end}}</h1>
      <p>{{if .Workspace}}{{.Workspace.Description}}{{else}}Manage workspaces and the markdown source files behind the public Trait library.{{end}}</p>
    </div>
    <div class="admin-head-actions">
      <details class="import-menu">
        <summary class="button secondary">Import markdown</summary>
        <div class="import-panel">
          <form method="post" action="/admin/import" enctype="multipart/form-data">
            {{if $.Workspace}}<input type="hidden" name="workspace" value="{{$.Workspace.Slug}}">{{end}}
            <label>Single trait<input type="file" name="traits" accept=".md,text/markdown" required></label>
            <button class="button" type="submit">Upload file</button>
          </form>
          <form method="post" action="/admin/import" enctype="multipart/form-data">
            {{if $.Workspace}}<input type="hidden" name="workspace" value="{{$.Workspace.Slug}}">{{end}}
            <label>Folder of traits<input type="file" name="traits" accept=".md,text/markdown" webkitdirectory directory multiple required></label>
            <button class="button" type="submit">Upload folder</button>
          </form>
          <p>Markdown filenames may repeat. Collisions are renamed automatically.</p>
        </div>
      </details>
      <a class="button" href="/admin/new{{if .Workspace}}?workspace={{.Workspace.Slug}}{{end}}">New trait</a>
    </div>
  </section>
  {{if .Notice}}<p class="notice" role="status">{{.Notice}}</p>{{end}}
  {{if not .Workspace}}
  <section class="workspace-admin-head"><h2>Workspaces</h2><a class="button secondary" href="/admin/workspaces/new">New workspace</a></section>
  <section class="workspace-grid">
    {{range .Workspaces}}<a class="workspace-card" href="/admin/workspaces/{{.Slug}}"><strong>{{.Name}}</strong><p>{{.Description}}</p><span>{{.TraitCount}} traits</span></a>{{else}}<p class="empty">No workspaces yet.</p>{{end}}
  </section>
  {{else}}<p><a class="back-link" href="/admin">Back to all workspaces and traits</a> · <a class="back-link" href="/workspaces/{{.Workspace.Slug}}">View public workspace</a></p>{{end}}
  <section class="admin-toolbar" aria-label="Trait administration tools">
    <label>Search traits<input data-trait-search type="search" placeholder="Search title, tag, or markdown" autocomplete="off"></label>
    <span>{{len .Traits}} traits</span>
  </section>
  <details class="admin-filters">
    <summary>Filters</summary>
    {{template "tags" .}}
  </details>
  <section class="admin-list" aria-label="Admin trait list">
    <div class="admin-row admin-row-head" aria-hidden="true">
      <span>Trait</span><span>Tags</span><span>Updated</span><span>Actions</span>
    </div>
    {{range .Traits}}
      <article class="admin-row" data-search data-search-text="{{.Title}} {{joinTags .Tags}} {{.Content}}">
        <div class="admin-title">
          <h2><a href="{{traitPath .}}">{{.Title}}</a></h2>
          <span>{{.Slug}}</span>
        </div>
        {{template "contentTags" .Tags}}
        <time datetime="{{.ModTime}}">{{.ModTime.Format "Jan 2, 2006"}}</time>
        <div class="actions">
          <a class="button secondary" href="/admin/edit/{{.Slug}}{{if .WorkspaceSlug}}?workspace={{.WorkspaceSlug}}{{end}}">Edit</a>
          <details class="danger-menu">
            <summary aria-label="Delete {{.Title}}">Delete</summary>
            <form method="post" action="/admin/delete/{{.Slug}}{{if .WorkspaceSlug}}?workspace={{.WorkspaceSlug}}{{end}}">
              <p>Delete <strong>{{.Title}}</strong>? This cannot be undone.</p>
              <button class="danger" type="submit">Confirm delete</button>
            </form>
          </details>
        </div>
      </article>
    {{else}}
      <p class="empty">No traits yet.</p>
    {{end}}
    {{if .Traits}}<p class="empty" data-search-empty hidden>No traits match that search.</p>{{end}}
  </section>
{{template "bottom" .}}{{end}}

{{define "edit"}}{{template "top" .}}
  <section class="section-head edit-head">
    <div>
      <h1>{{if .Trait.Slug}}Edit trait{{else}}New trait{{end}}</h1>
      <p>Write the reusable guidance in markdown. Tags can be added inline with #tag-name.</p>
    </div>
    <a class="button secondary" href="/admin">Cancel</a>
  </section>
  <form class="editor" method="post">
    {{if .Error}}<p class="error">{{.Error}}</p>{{end}}
    {{if not .Trait.Slug}}<label>Title<input name="title" value="{{.Trait.Title}}" autocomplete="off"></label>{{end}}
    {{if .Workspace}}<input type="hidden" name="workspace" value="{{.Workspace.Slug}}">{{end}}
    <label>Markdown<textarea name="content" rows="24">{{.Trait.Content}}</textarea></label>
    <div class="form-actions"><a class="button secondary" href="/admin">Cancel</a><button class="button" type="submit">Save trait</button></div>
  </form>
{{template "bottom" .}}{{end}}

{{define "workspaceEdit"}}{{template "top" .}}
  <section class="section-head edit-head"><div><h1>New workspace</h1><p>Create a one-level collection for related traits.</p></div><a class="button secondary" href="/admin">Cancel</a></section>
  <form class="editor" method="post">
    {{if .Error}}<p class="error">{{.Error}}</p>{{end}}
    <label>Name<input name="name" value="{{.Workspace.Name}}" autocomplete="off" required></label>
    <label>Description<textarea name="description" rows="10" placeholder="Add any context, purpose, instructions, or notes you want people to see.">{{.Workspace.Description}}</textarea></label>
    <div class="form-actions"><a class="button secondary" href="/admin">Cancel</a><button class="button" type="submit">Create workspace</button></div>
  </form>
{{template "bottom" .}}{{end}}

{{define "login"}}{{template "top" .}}
  <section class="login-shell">
    <div class="login-note">
      <p class="eyebrow">Trait admin</p>
      <h1>Manage reusable application guidance.</h1>
      <p>Keep the public library focused, current, and easy to browse from markdown source.</p>
    </div>
    <form class="login" method="post" action="/login">
      <img class="login-logo" src="/assets/logo.png" alt="" width="160" height="96">
      <h2>Welcome back</h2>
      <p>Sign in to manage the Trait library.</p>
      {{if .Error}}<p class="error">{{.Error}}</p>{{end}}
      <label>Username<input name="username" autocomplete="username"></label>
      <label>Password<input name="password" type="password" autocomplete="current-password"></label>
      <button class="button" type="submit">Sign in</button>
    </form>
  </section>
{{template "bottom" .}}{{end}}
`

const css = `
:root { --accent: __ACCENT__; color-scheme: dark; }
* { box-sizing: border-box; }
html, body { min-height: 100%; }
body { margin: 0; background: #050505; color: #f5f5f5; font-family: ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; line-height: 1.5; }
body.browser-page { height: 100vh; overflow: hidden; }
body.has-cart main { padding-bottom: 0; }
.asset-loading { position: fixed; inset: 0; z-index: 100; display: grid; place-items: center; background: #050505; color: #d8d8d8; font-weight: 750; }
.assets-ready .asset-loading { display: none; }
a { color: inherit; text-decoration: none; }
a:hover { color: var(--accent); }
.topbar { display: flex; justify-content: space-between; align-items: center; min-height: 82px; padding: 10px clamp(18px, 4vw, 48px); border-bottom: 1px solid #202020; position: sticky; top: 0; z-index: 30; background: rgba(5,5,5,.92); backdrop-filter: blur(12px); }
.brand { color: var(--accent); font-weight: 800; font-size: 1.1rem; display: inline-flex; align-items: center; gap: 10px; min-width: 0; }
.brand img { width: 180px; height: 54px; object-fit: contain; display: block; flex: 0 0 180px; }
.topnav { display: flex; align-items: center; gap: 6px; margin-left: auto; }
.topnav a { padding: 9px 11px; border-radius: 6px; color: #d9d9d9; font-weight: 750; }
.topnav a:hover { background: #111; color: var(--accent); }
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
.showcase-hero { min-height: calc(100vh - 180px); display: grid; grid-template-columns: minmax(0, 1fr) minmax(340px, 480px); gap: clamp(28px, 5vw, 72px); align-items: center; padding: clamp(28px, 7vw, 86px) 0; }
.showcase-copy { max-width: 720px; }
.eyebrow { margin: 0 0 14px; color: var(--accent); font-weight: 800; text-transform: uppercase; letter-spacing: .08em; font-size: .78rem; }
.showcase-copy h1 { margin: 0 0 18px; font-size: clamp(4rem, 13vw, 10rem); line-height: .86; letter-spacing: 0; }
.showcase-copy p:not(.eyebrow) { color: #d0d0d0; font-size: clamp(1.05rem, 2vw, 1.35rem); max-width: 640px; margin: 0; }
.hero-actions { display: flex; flex-wrap: wrap; gap: 12px; margin-top: 28px; }
.showcase-panel { max-height: 520px; border: 1px solid #242424; border-radius: 8px; background: #0b0b0b; overflow: hidden; display: grid; grid-template-rows: auto minmax(0, 1fr); box-shadow: 0 24px 80px rgba(0,0,0,.34); }
.panel-head { display: flex; justify-content: space-between; gap: 12px; padding: 14px 16px; border-bottom: 1px solid #202020; color: #bdbdbd; font-weight: 800; }
.panel-head a { color: var(--accent); }
.featured-trait { min-height: 0; overflow: auto; padding: 20px; }
.featured-trait h2 { margin: 8px 0 14px; font-size: clamp(1.35rem, 2.2vw, 2rem); line-height: 1.1; }
.featured-doc { max-height: 250px; overflow: hidden; color: #d7d7d7; margin-bottom: 18px; position: relative; }
.featured-doc::after { content: ""; position: absolute; left: 0; right: 0; bottom: 0; height: 64px; background: linear-gradient(transparent, #0b0b0b); pointer-events: none; }
.featured-doc h1 { display: none; }
.featured-doc h2 { font-size: 1.05rem; margin: 18px 0 8px; }
.featured-doc p, .featured-doc li { color: #d7d7d7; }
.featured-doc pre { max-height: 150px; overflow: hidden; padding: 14px; background: #101010; border: 1px solid #242424; border-radius: 8px; }
.showcase-band { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 18px; padding: 28px 0 6px; border-top: 1px solid #202020; }
.showcase-band article { min-width: 0; }
.showcase-band h2 { margin: 0 0 8px; font-size: 1.08rem; }
.showcase-band p { margin: 0; color: #bdbdbd; }
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
.trait-list-item { display: grid; grid-template-columns: minmax(0, 1fr) auto; gap: 10px; align-items: center; padding: 12px; border: 1px solid #202020; border-radius: 8px; background: #0b0b0b; cursor: pointer; }
.trait-list-item.active { border-color: var(--accent); background: #0f1411; }
.trait-list-item.selected { border-color: rgba(54, 215, 131, .7); background: #102017; box-shadow: inset 3px 0 0 var(--accent); }
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
.select-toggle[aria-pressed="true"] { border-color: rgba(54, 215, 131, .8); background: var(--accent-soft); color: var(--accent); }
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
  .topnav { gap: 2px; }
  .topnav a { padding: 7px 8px; font-size: .92rem; }
  .topnav a[href="/logout"] { display: none; }
  .section-head, .row { align-items: flex-start; flex-direction: column; }
  main { width: min(100% - 28px, 1120px); padding-top: 24px; }
  .showcase-hero { min-height: auto; grid-template-columns: 1fr; padding: 34px 0; }
  .showcase-copy h1 { font-size: 4.6rem; }
  .showcase-panel { min-height: 340px; }
  .showcase-band { grid-template-columns: 1fr; gap: 20px; }
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
  .trait-list-item { grid-template-columns: minmax(0, 1fr); }
  .trait-list-item .icon-button { justify-self: start; }
  .actions { width: 100%; justify-content: flex-start; flex-wrap: wrap; }
  .cart-bar { align-items: stretch; flex-wrap: wrap; }
  .cart-bar p { flex-basis: 100%; order: 4; }
}

:root {
  --bg: #090a0b;
  --surface-1: #0f1113;
  --surface-2: #15181b;
  --surface-3: #1b1f23;
  --border-subtle: rgba(255, 255, 255, 0.08);
  --border-strong: rgba(255, 255, 255, 0.14);
  --text-primary: #f4f5f6;
  --text-secondary: #b4bac2;
  --text-muted: #747b84;
  --accent: __ACCENT__;
  --accent-hover: #48e493;
  --accent-soft: rgba(54, 215, 131, 0.12);
  --danger: #ff5d67;
  --danger-hover: #ff7079;
  --danger-soft: rgba(255, 93, 103, 0.12);
  --radius-sm: 8px;
  --radius-md: 12px;
  --radius-lg: 16px;
  --shadow-card: 0 20px 50px rgba(0, 0, 0, 0.28);
  --content-max: 1440px;
  --reading-max: 800px;
  color-scheme: dark;
}

html { background: var(--bg); }
body {
  background: var(--bg);
  color: var(--text-primary);
  font-family: Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
  line-height: 1.65;
}

body.browser-page { overflow: hidden; }
.asset-loading { background: var(--bg); color: var(--text-secondary); }
.visually-hidden {
  position: absolute;
  width: 1px;
  height: 1px;
  padding: 0;
  margin: -1px;
  overflow: hidden;
  clip: rect(0, 0, 0, 0);
  white-space: nowrap;
  border: 0;
}

a, button, input, textarea, select, summary { transition: background-color .18s ease, border-color .18s ease, color .18s ease, box-shadow .18s ease, transform .18s ease; }
a:focus-visible, button:focus-visible, input:focus-visible, textarea:focus-visible, select:focus-visible, summary:focus-visible {
  outline: none;
  border-color: var(--accent);
  box-shadow: 0 0 0 3px var(--accent-soft);
}

.topbar {
  min-height: 72px;
  width: 100%;
  max-width: none;
  padding: 0 max(16px, calc((100vw - var(--content-max)) / 2 + 32px));
  border-bottom: 1px solid var(--border-subtle);
  background: rgba(9, 10, 11, .9);
}
.brand img { width: 166px; height: 48px; flex-basis: 166px; }
.topnav { gap: 4px; }
.topnav a {
  color: var(--text-secondary);
  border-radius: var(--radius-sm);
  padding: 8px 11px;
  font-size: .9rem;
  font-weight: 700;
}
.topnav a:hover { background: var(--surface-2); color: var(--text-primary); }
.menu-toggle { display: none; }
.side-menu { background: var(--surface-1); border-left-color: var(--border-subtle); }
.side-menu-head { border-bottom-color: var(--border-subtle); color: var(--text-secondary); }
.side-menu a:hover { background: var(--surface-2); border-color: var(--border-subtle); }

main {
  width: min(var(--content-max), calc(100% - 64px));
  padding: 48px 0 72px;
}
.browser-page main {
  width: 100%;
  height: calc(100vh - 72px);
  padding: 0;
}

.button, button {
  min-height: 44px;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  gap: 8px;
  border-radius: var(--radius-sm);
  background: var(--accent);
  color: #06100a;
  border: 1px solid transparent;
  padding: 10px 18px;
  font-weight: 760;
  line-height: 1;
}
.button:hover, button:hover { background: var(--accent-hover); color: #06100a; transform: translateY(-1px); }
.secondary {
  background: var(--surface-2);
  color: var(--text-primary);
  border-color: var(--border-subtle);
}
.secondary:hover { background: var(--surface-3); color: var(--text-primary); border-color: var(--border-strong); }
.danger {
  background: transparent;
  color: var(--danger);
  border-color: transparent;
}
.danger:hover { background: var(--danger-soft); color: var(--danger-hover); }
.icon-button {
  width: 40px;
  min-height: 40px;
  padding: 0;
  border-radius: var(--radius-sm);
  background: var(--surface-2);
  color: var(--text-secondary);
  border: 1px solid var(--border-subtle);
  font-size: .72rem;
}
.icon-button:hover { background: var(--surface-3); color: var(--text-primary); }

label { color: var(--text-secondary); font-size: .86rem; font-weight: 650; }
input, textarea, select {
  min-height: 46px;
  background: var(--surface-1);
  color: var(--text-primary);
  border: 1px solid var(--border-subtle);
  border-radius: var(--radius-sm);
  padding: 11px 12px;
}
input::placeholder, textarea::placeholder { color: var(--text-muted); }
input:focus, textarea:focus, select:focus {
  outline: none;
  border-color: var(--accent);
  box-shadow: 0 0 0 3px var(--accent-soft);
}
textarea { line-height: 1.55; }

.eyebrow {
  color: var(--accent);
  font-size: .76rem;
  letter-spacing: .08em;
  text-transform: uppercase;
  font-weight: 800;
}
.text-link, .controls-link, .panel-head a, .filter-head a {
  color: var(--accent);
  font-weight: 750;
}
.empty { color: var(--text-muted); }
.error {
  color: var(--danger-hover);
  background: var(--danger-soft);
  border-radius: var(--radius-sm);
  margin: 0;
  padding: 10px 12px;
}

.showcase-hero {
  min-height: auto;
  grid-template-columns: minmax(0, 1.05fr) minmax(340px, 560px);
  gap: clamp(32px, 6vw, 88px);
  padding: 96px 0 56px;
}
.showcase-copy { max-width: 650px; }
.showcase-copy h1 {
  margin: 0 0 20px;
  max-width: 760px;
  font-size: clamp(3.25rem, 7vw, 6rem);
  line-height: .94;
  font-weight: 820;
}
.showcase-copy p:not(.eyebrow) {
  max-width: 560px;
  color: var(--text-secondary);
  font-size: 1.1rem;
}
.hero-actions { align-items: center; gap: 18px; margin-top: 32px; }
.showcase-panel {
  max-width: 640px;
  max-height: none;
  border: 1px solid var(--border-subtle);
  border-radius: var(--radius-lg);
  background: linear-gradient(180deg, var(--surface-2), var(--surface-1));
  box-shadow: var(--shadow-card);
  overflow: hidden;
}
.panel-head {
  padding: 16px 20px;
  border-bottom: 1px solid var(--border-subtle);
  color: var(--text-secondary);
  font-size: .82rem;
  text-transform: uppercase;
  letter-spacing: .06em;
}
.featured-trait { padding: 30px; overflow: visible; }
.featured-trait h2 {
  margin: 18px 0 12px;
  font-size: clamp(1.65rem, 3vw, 2.35rem);
  line-height: 1.08;
}
.featured-trait p { color: var(--text-secondary); margin: 0 0 18px; }
.featured-meta {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  margin-bottom: 24px;
  color: var(--text-muted);
  font-size: .82rem;
}
.featured-meta span {
  border-radius: 999px;
  background: var(--surface-3);
  padding: 5px 9px;
}
.showcase-band {
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: 16px;
  border-top: 0;
  padding: 8px 0 28px;
}
.showcase-band article {
  min-height: 188px;
  padding: 22px;
  border: 1px solid var(--border-subtle);
  border-radius: var(--radius-md);
  background: var(--surface-1);
}
.feature-icon {
  width: 34px;
  height: 34px;
  display: grid;
  place-items: center;
  border-radius: var(--radius-sm);
  background: var(--accent-soft);
  color: var(--accent);
  font-weight: 850;
  margin-bottom: 18px;
}
.showcase-band h2 { color: var(--text-primary); font-size: 1.12rem; }
.showcase-band p { color: var(--text-secondary); line-height: 1.55; }

.browser-shell {
  height: 100%;
  grid-template-rows: auto minmax(0, 1fr);
  gap: 0;
}
.browser-toolbar {
  min-height: 86px;
  align-items: center;
  padding: 16px max(16px, calc((100vw - var(--content-max)) / 2 + 32px));
  border-bottom: 1px solid var(--border-subtle);
  background: var(--bg);
}
.browser-toolbar .eyebrow { margin-bottom: 4px; }
.browser-toolbar h1 { font-size: clamp(1.7rem, 3vw, 2.55rem); }
.browser-toolbar p:not(.eyebrow) { color: var(--text-secondary); margin-top: 2px; }
.trait-browser {
  min-height: 0;
  grid-template-columns: minmax(240px, 280px) minmax(360px, 430px) minmax(0, 1fr);
  gap: 0;
}
.library-panel, .results-panel, .reader-panel {
  min-height: 0;
  overflow: auto;
  border-right: 1px solid var(--border-subtle);
}
.library-panel {
  display: block;
  padding: 20px;
  background: var(--surface-1);
}
.sidebar-head {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: 12px;
  margin-bottom: 18px;
}
.sidebar-head h2 { margin: 0; font-size: 1.15rem; line-height: 1.2; }
.sidebar-head span, .filter-head, .list-toolbar {
  color: var(--text-muted);
  font-size: .82rem;
  font-weight: 700;
}
.search {
  grid-template-columns: 1fr;
  gap: 10px;
  margin: 0 0 20px;
}
.filter-head {
  display: flex;
  justify-content: space-between;
  gap: 12px;
  margin-bottom: 10px;
}
.tags {
  gap: 8px;
  margin: 0;
  max-height: min(42vh, 440px);
  overflow: auto;
  align-content: start;
}
.tag {
  border-radius: 999px;
  border: 1px solid var(--border-subtle);
  background: var(--surface-2);
  color: var(--text-secondary);
  padding: 7px 10px;
  font-size: .85rem;
  line-height: 1;
}
.tag:hover { border-color: var(--border-strong); background: var(--surface-3); color: var(--text-primary); }
.tag.active {
  border-color: rgba(54, 215, 131, .35);
  background: var(--accent-soft);
  color: var(--accent);
}
.results-panel {
  display: grid;
  grid-template-rows: auto minmax(0, 1fr);
  background: #0c0d0f;
}
.list-toolbar {
  min-height: 56px;
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 12px 16px;
  border-bottom: 1px solid var(--border-subtle);
}
.bulk-actions {
  display: none;
  align-items: center;
  gap: 8px;
}
.has-cart .bulk-actions { display: flex; }
.bulk-actions .button, .bulk-actions button { min-height: 36px; padding: 8px 11px; font-size: .82rem; }
.trait-list {
  gap: 10px;
  padding: 16px;
}
.trait-list-item {
  grid-template-columns: minmax(0, 1fr) auto;
  align-items: start;
  gap: 12px;
  padding: 16px;
  border: 1px solid var(--border-subtle);
  border-radius: var(--radius-md);
  background: var(--surface-1);
  cursor: pointer;
}
.trait-list-item:hover { background: var(--surface-2); transform: translateY(-1px); }
.trait-list-item.active {
  border-color: rgba(54, 215, 131, .4);
  background: var(--accent-soft);
  box-shadow: inset 3px 0 0 var(--accent);
}
.trait-list-item.selected {
  border-color: rgba(54, 215, 131, .72);
  background: rgba(54, 215, 131, .12);
  box-shadow: inset 3px 0 0 var(--accent);
}
.trait-link strong {
  color: var(--text-primary);
  font-size: 1rem;
  line-height: 1.28;
}
.content-tags {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
}
.content-tag {
  border-radius: 999px;
  background: rgba(255, 255, 255, .045);
  color: #90d9b1;
  padding: 4px 7px;
  font-size: .75rem;
  line-height: 1;
}
.select-toggle[aria-pressed="true"] {
  border-color: rgba(54, 215, 131, .8);
  background: var(--accent-soft);
  color: var(--accent);
}
.reader-panel {
  border-right: 0;
  border-left: 0;
  padding: 0;
  background: var(--bg);
}
.reader-head {
  max-width: var(--reading-max);
  margin: 0 auto;
  padding: 36px 32px 24px;
  border-bottom: 1px solid var(--border-subtle);
}
.reader-head h1 {
  margin: 14px 0 14px;
  font-size: clamp(1.9rem, 3.4vw, 2.85rem);
  line-height: 1.04;
}
.reader-head > div:last-child {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 10px;
  margin-top: 20px;
}
.back-link {
  display: inline-flex;
  color: var(--text-secondary);
  font-size: .86rem;
}
.article-body {
  max-width: var(--reading-max);
  margin: 0 auto;
  padding: 30px 32px 80px;
}
.article-body > h1:first-child { display: none; }
.doc h1, .doc h2, .doc h3 { color: var(--text-primary); line-height: 1.18; }
.doc h2 { margin: 34px 0 12px; font-size: 1.55rem; }
.doc h3 { margin: 26px 0 10px; font-size: 1.18rem; }
.doc p, .doc li { color: var(--text-secondary); }
.doc p { margin: 0 0 18px; }
.doc ul, .doc ol { padding-left: 24px; margin: 0 0 20px; }
.doc li + li { margin-top: 8px; }
.doc pre {
  background: var(--surface-1);
  border: 1px solid var(--border-subtle);
  border-radius: var(--radius-md);
}
.doc code {
  border-radius: 5px;
  background: var(--surface-2);
  padding: 2px 5px;
}
.doc pre code { background: transparent; padding: 0; }
.cart-bar {
  width: min(620px, calc(100% - 32px));
  padding: 10px 14px;
  border-color: var(--border-subtle);
  border-radius: var(--radius-md);
  background: rgba(15, 17, 19, .96);
}

.section-head {
  align-items: center;
  margin-bottom: 28px;
}
.section-head h1 {
  margin: 0 0 8px;
  font-size: clamp(2rem, 4vw, 3.25rem);
  line-height: 1.05;
}
.section-head p { color: var(--text-secondary); }
.admin-head, .edit-head, .admin-toolbar, .admin-filters, .admin-list, .editor {
  max-width: 1180px;
  margin-left: auto;
  margin-right: auto;
}
.admin-head-actions { display: flex; align-items: center; gap: 10px; }
.import-menu { position: relative; }
.import-menu > summary { list-style: none; cursor: pointer; }
.import-menu > summary::-webkit-details-marker { display: none; }
.import-panel {
  position: absolute;
  z-index: 20;
  top: calc(100% + 8px);
  right: 0;
  width: min(420px, calc(100vw - 32px));
  padding: 16px;
  border: 1px solid var(--border-strong);
  border-radius: var(--radius-md);
  background: var(--surface-2);
  box-shadow: 0 18px 48px rgba(0, 0, 0, .42);
}
.import-panel form { display: grid; grid-template-columns: minmax(0, 1fr) auto; align-items: end; gap: 10px; }
.import-panel form + form { margin-top: 14px; padding-top: 14px; border-top: 1px solid var(--border-subtle); }
.import-panel input[type="file"] { display: block; width: 100%; margin-top: 6px; color: var(--text-secondary); }
.import-panel p { margin: 12px 0 0; color: var(--text-muted); font-size: .8rem; }
.notice { margin: 0 auto 18px; padding: 12px 14px; border: 1px solid rgba(54, 215, 131, .35); border-radius: var(--radius-md); background: var(--accent-soft); color: var(--text-primary); }
.workspace-context, .workspace-links { margin: 0 16px 16px; padding: 12px; border: 1px solid var(--border-subtle); border-radius: var(--radius-md); background: var(--surface-2); }
.workspace-context p { margin: 6px 0 10px; color: var(--text-secondary); white-space: pre-wrap; }
.workspace-links > span { display: block; margin-bottom: 8px; color: var(--text-muted); font-size: .75rem; text-transform: uppercase; letter-spacing: .08em; }
.workspace-links a { display: flex; justify-content: space-between; gap: 8px; padding: 8px 0; color: var(--text-primary); }
.workspace-links small { color: var(--text-muted); }
.workspace-admin-head { display: flex; align-items: center; justify-content: space-between; margin: 20px 0 10px; }
.workspace-admin-head h2 { margin: 0; }
.workspace-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(230px, 1fr)); gap: 12px; margin-bottom: 24px; }
.workspace-card { padding: 16px; border: 1px solid var(--border-subtle); border-radius: var(--radius-md); background: var(--surface-1); color: var(--text-primary); }
.workspace-card:hover { border-color: var(--border-strong); background: var(--surface-2); }
.workspace-card strong { font-size: 1.05rem; }
.workspace-card p { min-height: 2.5em; margin: 8px 0; color: var(--text-secondary); white-space: pre-wrap; }
.workspace-card span { color: var(--text-muted); font-size: .82rem; }
.admin-toolbar {
  display: grid;
  grid-template-columns: minmax(260px, 520px) 1fr;
  align-items: end;
  gap: 18px;
  margin-bottom: 14px;
}
.admin-toolbar > span {
  justify-self: end;
  color: var(--text-muted);
  font-weight: 700;
}
.admin-filters {
  margin-bottom: 18px;
  border: 1px solid var(--border-subtle);
  border-radius: var(--radius-md);
  background: var(--surface-1);
}
.admin-filters summary {
  cursor: pointer;
  padding: 12px 14px;
  color: var(--text-secondary);
  font-weight: 750;
}
.admin-filters .tags { padding: 0 14px 14px; max-height: 128px; }
.admin-list {
  display: grid;
  gap: 0;
  overflow: visible;
  border: 1px solid var(--border-subtle);
  border-radius: var(--radius-md);
  background: var(--surface-1);
}
.admin-row {
  min-height: 72px;
  display: grid;
  grid-template-columns: minmax(240px, 1.4fr) minmax(180px, 1fr) 130px 160px;
  align-items: center;
  gap: 16px;
  padding: 12px 16px;
  border-bottom: 1px solid var(--border-subtle);
}
.admin-row:last-child { border-bottom: 0; }
.admin-row[hidden] { display: none; }
.admin-row-head {
  min-height: 44px;
  color: var(--text-muted);
  font-size: .76rem;
  font-weight: 800;
  text-transform: uppercase;
  letter-spacing: .06em;
  background: var(--surface-2);
}
.admin-title h2 { margin: 0 0 3px; font-size: 1rem; line-height: 1.25; }
.admin-title span, .admin-row time { color: var(--text-muted); font-size: .82rem; }
.actions { justify-content: flex-end; }
.actions .button, .actions button { min-height: 38px; padding: 8px 12px; font-size: .86rem; }
.danger-menu { position: relative; }
.danger-menu summary {
  list-style: none;
  min-height: 38px;
  display: inline-flex;
  align-items: center;
  border-radius: var(--radius-sm);
  color: var(--danger);
  padding: 8px 10px;
  cursor: pointer;
}
.danger-menu summary::-webkit-details-marker { display: none; }
.danger-menu summary:hover { background: var(--danger-soft); }
.danger-menu form {
  position: absolute;
  right: 0;
  top: calc(100% + 8px);
  z-index: 10;
  width: 280px;
  margin: 0;
  padding: 14px;
  border: 1px solid var(--border-strong);
  border-radius: var(--radius-md);
  background: var(--surface-2);
  box-shadow: var(--shadow-card);
}
.danger-menu p { margin: 0 0 12px; color: var(--text-secondary); font-size: .9rem; }
.danger-menu .danger {
  width: 100%;
  background: var(--danger);
  color: #190305;
}
.editor {
  max-width: 760px;
  gap: 18px;
  padding: 28px;
  border: 1px solid var(--border-subtle);
  border-radius: var(--radius-lg);
  background: var(--surface-1);
}
.editor textarea { min-height: 540px; }
.form-actions {
  position: sticky;
  bottom: 0;
  display: flex;
  justify-content: flex-end;
  gap: 10px;
  padding-top: 14px;
  background: var(--surface-1);
}

.login-shell {
  min-height: calc(100vh - 72px - 96px);
  display: grid;
  grid-template-columns: minmax(0, 1fr) 460px;
  align-items: center;
  gap: clamp(32px, 7vw, 96px);
  max-width: 1040px;
  margin: 0 auto;
}
.login-note h1 {
  margin: 0 0 16px;
  font-size: clamp(2.3rem, 4.6vw, 4.2rem);
  line-height: .98;
}
.login-note p:not(.eyebrow) {
  max-width: 520px;
  color: var(--text-secondary);
  font-size: 1.05rem;
}
.login {
  width: 100%;
  max-width: 460px;
  margin: 0;
  gap: 16px;
  padding: 40px;
  border: 1px solid var(--border-subtle);
  border-radius: var(--radius-lg);
  background: var(--surface-1);
  box-shadow: var(--shadow-card);
}
.login-logo {
  width: 158px;
  height: 88px;
  object-fit: contain;
  margin-bottom: 4px;
}
.login h2 {
  margin: 0;
  font-size: 2rem;
  line-height: 1.1;
}
.login > p { margin: 0; color: var(--text-secondary); }
.login .button { width: 100%; margin-top: 4px; }

@media (prefers-reduced-motion: reduce) {
  *, *::before, *::after {
    transition-duration: .01ms !important;
    scroll-behavior: auto !important;
  }
}

@media (max-width: 1199px) {
  .trait-browser { grid-template-columns: minmax(260px, 330px) minmax(0, 1fr); }
  .library-panel { display: none; }
  .browser-toolbar { padding-left: 24px; padding-right: 24px; }
  .reader-head, .article-body { padding-left: 28px; padding-right: 28px; }
  .admin-row { grid-template-columns: minmax(220px, 1.4fr) minmax(170px, 1fr) 120px 150px; }
}

@media (max-width: 767px) {
  body.browser-page { overflow: auto; }
  .topbar {
    min-height: 68px;
    padding-left: 16px;
    padding-right: 16px;
  }
  .brand img { width: 142px; height: 42px; flex-basis: 142px; }
  .topnav { display: none; }
  .menu-toggle { display: inline-grid; }
  main { width: min(100% - 32px, var(--content-max)); padding: 32px 0 56px; }
  .browser-page main { height: auto; min-height: calc(100vh - 68px); }
  .showcase-hero {
    grid-template-columns: 1fr;
    padding: 64px 0 36px;
  }
  .showcase-copy h1 { font-size: clamp(2.8rem, 14vw, 4.4rem); }
  .showcase-band { grid-template-columns: 1fr; }
  .showcase-band article { min-height: auto; }
  .browser-toolbar {
    min-height: auto;
    align-items: flex-start;
    padding: 16px;
  }
  .browser-toolbar p:not(.eyebrow) { display: none; }
  .trait-browser {
    display: block;
    min-height: auto;
  }
  .results-panel, .reader-panel {
    border-right: 0;
    min-height: auto;
    overflow: visible;
  }
  .results-panel { display: block; }
  .list-toolbar { position: sticky; top: 68px; z-index: 5; background: #0c0d0f; }
  .trait-list { padding: 12px 16px 20px; }
  .trait-list-item { grid-template-columns: minmax(0, 1fr) auto; }
  .detail-page .results-panel { display: none; }
  .reader-head, .article-body {
    max-width: 100%;
    padding-left: 18px;
    padding-right: 18px;
  }
  .reader-head h1 { font-size: 2rem; }
  body:not(.detail-page) .reader-panel { display: none; }
  .bulk-actions {
    position: fixed;
    left: 12px;
    right: 12px;
    bottom: 12px;
    z-index: 20;
    justify-content: center;
    padding: 10px;
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-md);
    background: rgba(15, 17, 19, .96);
    box-shadow: var(--shadow-card);
  }
  .admin-head, .section-head {
    align-items: flex-start;
    flex-direction: column;
  }
  .admin-toolbar { grid-template-columns: 1fr; }
  .admin-toolbar > span { justify-self: start; }
  .admin-row-head { display: none; }
  .admin-list {
    gap: 10px;
    border: 0;
    background: transparent;
  }
  .admin-row {
    grid-template-columns: 1fr;
    min-height: 0;
    gap: 12px;
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-md);
    background: var(--surface-1);
  }
  .actions { justify-content: flex-start; flex-wrap: wrap; }
  .danger-menu form { left: 0; right: auto; width: min(280px, calc(100vw - 48px)); }
  .editor { padding: 20px; }
  .form-actions { flex-direction: column-reverse; }
  .form-actions .button, .form-actions button { width: 100%; }
  .login-shell {
    min-height: auto;
    grid-template-columns: 1fr;
    gap: 24px;
  }
  .login-note h1 { font-size: 2.45rem; }
  .login { padding: 28px; }
}

/* Responsive landing-page layout */
:root {
  --page-max-width: 1440px;
  --header-height: 104px;
  --page-gutter: clamp(24px, 4vw, 48px);
}

*,
*::before,
*::after {
  box-sizing: border-box;
}

html,
body {
  overflow-x: clip;
}

.page-container {
  width: min(calc(100% - (var(--page-gutter) * 2)), var(--page-max-width));
  margin-inline: auto;
}

.site-header {
  min-height: var(--header-height);
  padding: 0;
}

.site-header-inner {
  min-height: var(--header-height);
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 32px;
}

.site-logo,
.site-nav {
  min-width: 0;
}

.showcase-page main {
  width: min(calc(100% - (var(--page-gutter) * 2)), var(--page-max-width));
  max-width: 100%;
  margin-inline: auto;
  padding: 0 0 72px;
}

.showcase-page .hero {
  display: grid;
  grid-template-columns: minmax(0, 3fr) minmax(420px, 2fr);
  gap: clamp(48px, 7vw, 112px);
  align-items: center;
  min-height: calc(100svh - var(--header-height));
  padding-block: clamp(64px, 9vw, 132px);
  margin: 0;
  border: 0;
}

.hero-copy,
.featured-trait-card,
.featured-trait-body {
  min-width: 0;
  max-width: 100%;
}

.showcase-copy.hero-copy {
  max-width: none;
}

.showcase-copy .hero-title {
  margin: 24px 0 28px;
  max-width: 10ch;
  font-size: clamp(4rem, 6vw, 6rem);
  line-height: .92;
  letter-spacing: -.055em;
  text-wrap: balance;
  overflow-wrap: anywhere;
}

.showcase-copy p.hero-description {
  max-width: 34rem;
  margin: 0 0 40px;
  color: var(--text-secondary);
  font-size: clamp(1.1rem, 1.5vw, 1.45rem);
  line-height: 1.65;
}

.hero-actions {
  margin-top: 0;
}

.showcase-panel.featured-trait-card {
  width: 100%;
  max-width: 860px;
  max-height: none;
  justify-self: end;
  border-color: rgba(255, 255, 255, .07);
  border-radius: 24px;
  background: linear-gradient(145deg, rgba(24, 28, 31, .92), rgba(15, 17, 19, .98));
  overflow: hidden;
}

.featured-trait-header {
  min-height: 58px;
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 24px;
  flex-wrap: wrap;
}

.featured-trait.featured-trait-body {
  padding: clamp(28px, 4vw, 52px);
  overflow: visible;
}

.featured-trait .featured-trait-title {
  font-size: clamp(2rem, 3vw, 3rem);
  line-height: 1.1;
  overflow-wrap: anywhere;
}

.featured-trait-body .content-tags,
.featured-trait-body .featured-meta {
  display: flex;
  flex-wrap: wrap;
}

@media (max-width: 1150px) and (min-width: 1101px) {
  .showcase-copy .hero-title {
    font-size: clamp(3.75rem, 8vw, 6rem);
    max-width: 10ch;
  }
}

@media (max-width: 1100px) {
  .showcase-page .hero {
    grid-template-columns: minmax(0, 1fr);
    gap: 56px;
    align-items: start;
    min-height: auto;
    padding-block: 64px 96px;
  }

  .showcase-copy.hero-copy {
    max-width: 760px;
  }

  .showcase-copy .hero-title {
    max-width: 10ch;
    font-size: clamp(3.75rem, 8vw, 6rem);
  }

  .showcase-panel.featured-trait-card {
    width: 100%;
    max-width: none;
    justify-self: stretch;
  }
}

@media (max-width: 700px) {
  :root {
    --header-height: 80px;
    --page-gutter: 16px;
  }

  .site-header,
  .site-header-inner {
    min-height: var(--header-height);
  }

  .site-header-inner {
    gap: 20px;
  }

  .site-logo {
    max-width: 160px;
  }

  .site-logo img {
    max-width: 100%;
  }

  .showcase-page .hero {
    gap: 40px;
    padding-block: 48px 72px;
  }

  .showcase-copy .hero-title {
    max-width: 100%;
    font-size: clamp(3rem, 15vw, 4.75rem);
    line-height: .96;
  }

  .showcase-copy p.hero-description {
    font-size: 1.05rem;
    line-height: 1.6;
  }

  .featured-trait.featured-trait-body {
    padding: 28px 22px;
  }
}

@media (max-width: 480px) {
  .site-logo {
    max-width: 142px;
  }

  .site-nav {
    display: none;
  }

  .menu-toggle {
    display: inline-grid;
    flex: 0 0 auto;
  }

  .hero-actions {
    align-items: stretch;
    flex-direction: column;
  }

  .hero-actions .button,
  .hero-actions .text-link,
  .featured-trait-body > .button {
    width: 100%;
    text-align: center;
    justify-content: center;
  }

  .featured-trait-header {
    align-items: flex-start;
    flex-direction: column;
    gap: 8px;
  }
}
`
