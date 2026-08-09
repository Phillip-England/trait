package main

import (
	"bytes"
	"database/sql"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yuin/goldmark"
)

func TestAdminImportFileAndDirectoryResolveCollisions(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "shared-name.md"), []byte("# Shared Name\n\nexisting\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	files := []struct{ name, content string }{
		{"one.md", "# Shared Name\n\nfirst upload"},
		{"nested/two.md", "# Shared Name\n\nsecond upload"},
		{"nested/no-heading.md", "body without a heading"},
		{"nested/ignored.txt", "not markdown"},
	}
	for _, item := range files {
		part, err := writer.CreateFormFile("traits", item.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write([]byte(item.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/import", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	res := httptest.NewRecorder()
	app := &App{cfg: Config{TraitsDir: dir}}
	app.adminImport(res, req)

	if res.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body.String())
	}
	for _, name := range []string{"shared-name.md", "shared-name-2.md", "shared-name-3.md", "no-heading.md"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("expected %s: %v", name, err)
		}
	}
	generated, err := os.ReadFile(filepath.Join(dir, "no-heading.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(generated), "# no-heading\n\n") {
		t.Fatalf("missing generated heading: %q", generated)
	}
	location := res.Header().Get("Location")
	if !strings.Contains(location, "Imported+3+traits") || !strings.Contains(location, "skipped+1") {
		t.Errorf("unexpected redirect notice: %s", location)
	}
}

func TestWorkspaceCreationAndPublicSearch(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite3", filepath.Join(dir, "test.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := initDB(db); err != nil {
		t.Fatal(err)
	}
	app := &App{cfg: Config{TraitsDir: filepath.Join(dir, "traits"), AccentColor: "#35d07f"}, db: db, md: goldmark.New()}
	if err := os.MkdirAll(app.cfg.TraitsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	_, err = db.Exec(`INSERT INTO workspaces (slug, name, description, repository_url, created_at) VALUES ('platform-ideas', 'Platform Ideas', 'Notes for the platform', 'https://example.com/platform.git', 1)`)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(app.workspaceDir("platform-ideas"), 0o755); err != nil {
		t.Fatal(err)
	}
	workspace, err := app.loadWorkspace("platform-ideas")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writeUniqueTrait(app.workspaceDir(workspace.Slug), "deploy-safely", []byte("# Deploy Safely\n\n#release\n")); err != nil {
		t.Fatal(err)
	}
	if err := app.setTraitCategory(workspace.Slug, "deploy-safely", "Operations"); err != nil {
		t.Fatal(err)
	}

	publicReq := httptest.NewRequest(http.MethodGet, "/workspaces/platform-ideas?q=deploy&category=Operations", nil)
	publicRes := httptest.NewRecorder()
	app.publicWorkspace(publicRes, publicReq)
	if publicRes.Code != http.StatusOK {
		t.Fatalf("public workspace: status=%d body=%s", publicRes.Code, publicRes.Body.String())
	}
	for _, expected := range []string{"Platform Ideas", "Notes for the platform", "Deploy Safely", "Operations", "#release"} {
		if !strings.Contains(publicRes.Body.String(), expected) {
			t.Errorf("public workspace missing %q", expected)
		}
	}
}

func TestRegisterRepositoryImportsTraitsAndRejectsMissingFolder(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite3", filepath.Join(dir, "test.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := initDB(db); err != nil {
		t.Fatal(err)
	}
	app := &App{cfg: Config{TraitsDir: filepath.Join(dir, "traits"), AccentColor: "#35d07f"}, db: db, md: goldmark.New()}
	app.cloneRepository = func(_ string, destination string) error {
		if err := os.MkdirAll(filepath.Join(destination, "traits", "nested"), 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(destination, "traits", "nested", "safe-deploy.md"), []byte("# Safe Deploy\n\n#release"), 0o644)
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/repositories/new", strings.NewReader("name=Platform&repository_url=https%3A%2F%2Fexample.com%2Fplatform.git"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res := httptest.NewRecorder()
	app.adminWorkspaceNew(res, req)
	if res.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
	}
	if location := res.Header().Get("Location"); !strings.Contains(location, "Imported+1+trait") {
		t.Fatalf("redirect does not confirm clone/import: %s", location)
	}
	if _, err := os.Stat(filepath.Join(app.workspaceDir("platform"), "safe-deploy.md")); err != nil {
		t.Fatal(err)
	}

	app.cloneRepository = func(_ string, destination string) error { return os.MkdirAll(destination, 0o755) }
	req = httptest.NewRequest(http.MethodPost, "/admin/repositories/new", strings.NewReader("name=Empty&repository_url=https%3A%2F%2Fexample.com%2Fempty.git"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res = httptest.NewRecorder()
	app.adminWorkspaceNew(res, req)
	if !strings.Contains(res.Body.String(), "does not have a ./traits folder") {
		t.Fatalf("body=%s", res.Body.String())
	}
}

func TestRepositoryAdminPageHasNavigationAndAddAction(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite3", filepath.Join(dir, "test.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := initDB(db); err != nil {
		t.Fatal(err)
	}
	app := &App{cfg: Config{TraitsDir: filepath.Join(dir, "traits"), AccentColor: "#35d07f"}, db: db}
	res := httptest.NewRecorder()
	app.adminRepositories(res, httptest.NewRequest(http.MethodGet, "/admin/repositories", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
	}
	for _, expected := range []string{"Repositories", "/admin/repositories/new", "Add your first repository"} {
		if !strings.Contains(res.Body.String(), expected) {
			t.Errorf("repository admin page missing %q", expected)
		}
	}
}

func TestPublicLibraryShowsRepositoriesUntilSearch(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite3", filepath.Join(dir, "test.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := initDB(db); err != nil {
		t.Fatal(err)
	}
	app := &App{cfg: Config{TraitsDir: filepath.Join(dir, "traits"), AccentColor: "#35d07f"}, db: db, md: goldmark.New()}
	if err := os.MkdirAll(app.cfg.TraitsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO workspaces (slug, name, description, repository_url, created_at) VALUES ('source', 'Source Repo', '', 'https://example.com/source.git', 1)`)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(app.workspaceDir("source"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app.workspaceDir("source"), "hidden.md"), []byte("# Hidden Trait\n\nneedle"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := httptest.NewRecorder()
	app.publicTraits(res, httptest.NewRequest(http.MethodGet, "/traits", nil))
	if strings.Contains(res.Body.String(), "Hidden Trait") || !strings.Contains(res.Body.String(), "Source Repo") {
		t.Fatalf("body=%s", res.Body.String())
	}
	res = httptest.NewRecorder()
	app.publicTraits(res, httptest.NewRequest(http.MethodGet, "/traits?q=needle", nil))
	if !strings.Contains(res.Body.String(), "Hidden Trait") {
		t.Fatalf("body=%s", res.Body.String())
	}
}

func TestTraitDumpGuideRequiresPortableBehavior(t *testing.T) {
	for _, expected := range []string{"different codebase", "application-level idea", "Acceptance Checks", "categories belong to the destination workspace"} {
		if !strings.Contains(traitDumpGuide, expected) {
			t.Errorf("dump guide missing %q", expected)
		}
	}
	for _, rejected := range []string{"## Implementation\n", "## Project Evidence\n"} {
		if strings.Contains(traitDumpGuide, rejected) {
			t.Errorf("dump guide still requires source-specific section %q", rejected)
		}
	}
}

func TestWorkspaceCategoryTemplateRenders(t *testing.T) {
	res := httptest.NewRecorder()
	render(res, "public", PageData{
		Config:         Config{AccentColor: "#35d07f"},
		Workspace:      Workspace{Slug: "product", Name: "Product"},
		Categories:     []string{"User experience"},
		ActiveCategory: "User experience",
		Traits:         []Trait{{Slug: "clear-errors", Title: "Clear Errors", Category: "User experience"}},
	})
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "User experience") {
		t.Fatalf("category template did not render: status=%d body=%s", res.Code, res.Body.String())
	}
}

func TestAdminImportRejectsUploadWithoutMarkdown(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("traits", "notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("hello"))
	_ = writer.Close()

	req := httptest.NewRequest(http.MethodPost, "/admin/import", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	res := httptest.NewRecorder()
	app := &App{cfg: Config{TraitsDir: t.TempDir()}}
	app.adminImport(res, req)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body.String())
	}
}
