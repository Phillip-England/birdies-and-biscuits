package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"net/mail"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const (
	sessionCookieName = "bab_session"
	sessionLifetime   = 12 * time.Hour
	loginWindow       = 24 * time.Hour
	maxLoginFailures  = 5
	maxCSVBytes       = 5 << 20
	defaultListenAddr = ":8777"
	defaultEnvPath    = "config/.env"
	defaultDBPath     = "data/main.sqlite"
)

type Config struct {
	AdminUsername string
	AdminPassword string
	SessionSecret string
	DBPath        string
}

type App struct {
	cfg      Config
	db       *sql.DB
	sessions *SessionStore
	tpl      *template.Template
}

type SessionStore struct {
	mu       sync.Mutex
	sessions map[string]time.Time
}

type Member struct {
	ID         int64  `json:"id"`
	FirstName  string `json:"firstName"`
	LastName   string `json:"lastName"`
	Role       string `json:"role"`
	City       string `json:"city"`
	State      string `json:"state"`
	Handicap   string `json:"handicap"`
	HomeCourse string `json:"homeCourse"`
	GuestFee   string `json:"guestFee"`
	Bio        string `json:"bio"`
	Email      string `json:"email"`
	Phone      string `json:"phone"`
	ImportedAt int64  `json:"importedAt"`
}

type ImportSummary struct {
	Count      int
	ImportedAt int64
}

type PageData struct {
	Title     string
	Error     string
	Message   string
	IsAuthed  bool
	Members   template.JS
	Summary   ImportSummary
	StartedAt string
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
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintf(os.Stderr, "  birdies-and-biscuits init [-env %s] [-db %s]\n", defaultEnvPath, defaultDBPath)
	fmt.Fprintf(os.Stderr, "  birdies-and-biscuits serve [-env %s] [-addr %s]\n", defaultEnvPath, defaultListenAddr)
}

func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	envPath := fs.String("env", defaultEnvPath, "path to create env file")
	dbPath := fs.String("db", defaultDBPath, "path to create SQLite database")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*envPath) == "" || strings.TrimSpace(*dbPath) == "" {
		return errors.New("env and db paths cannot be empty")
	}
	absEnv, err := filepath.Abs(*envPath)
	if err != nil {
		return err
	}
	absDB, err := filepath.Abs(*dbPath)
	if err != nil {
		return err
	}
	if _, err := os.Stat(absEnv); err == nil {
		return fmt.Errorf("%s already exists", absEnv)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	secret, err := randomToken(48)
	if err != nil {
		return err
	}
	dbConfigPath, err := filepath.Rel(filepath.Dir(absEnv), absDB)
	if err != nil {
		dbConfigPath = absDB
	}
	content := fmt.Sprintf("ADMIN_USERNAME=admin\nADMIN_PASSWORD=change-me-now\nSESSION_SECRET=%s\nDB_PATH=%s\n", secret, dbConfigPath)
	if err := os.MkdirAll(filepath.Dir(absEnv), 0755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(absDB), 0755); err != nil {
		return err
	}
	if err := os.WriteFile(absEnv, []byte(content), 0600); err != nil {
		return err
	}
	db, err := sql.Open("sqlite", absDB)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := migrate(db); err != nil {
		return fmt.Errorf("initialize database: %w", err)
	}
	fmt.Printf("created %s\ninitialized %s\n", absEnv, absDB)
	return nil
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	envPath := fs.String("env", defaultEnvPath, "path to env file")
	addr := fs.String("addr", defaultListenAddr, "address to listen on")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*envPath) == "" {
		return errors.New("env path cannot be empty")
	}
	cfg, err := loadConfig(*envPath)
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", cfg.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := migrate(db); err != nil {
		return err
	}
	app := &App{
		cfg:      cfg,
		db:       db,
		sessions: NewSessionStore(),
		tpl:      template.Must(template.New("pages").Parse(pageTemplates)),
	}
	server := &http.Server{
		Addr:              *addr,
		Handler:           app.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("birdies-and-biscuits listening on http://localhost%s", *addr)
	return server.ListenAndServe()
}

func loadConfig(envPath string) (Config, error) {
	absEnv, err := filepath.Abs(envPath)
	if err != nil {
		return Config{}, err
	}
	raw, err := os.ReadFile(absEnv)
	if err != nil {
		return Config{}, fmt.Errorf("read env file: %w", err)
	}
	values := map[string]string{}
	lines := strings.Split(string(raw), "\n")
	for idx, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return Config{}, fmt.Errorf("invalid env line %d", idx+1)
		}
		values[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `"'`)
	}
	cfg := Config{
		AdminUsername: values["ADMIN_USERNAME"],
		AdminPassword: values["ADMIN_PASSWORD"],
		SessionSecret: values["SESSION_SECRET"],
		DBPath:        values["DB_PATH"],
	}
	if cfg.AdminUsername == "" || cfg.AdminPassword == "" || cfg.SessionSecret == "" || cfg.DBPath == "" {
		return Config{}, errors.New("env file must define ADMIN_USERNAME, ADMIN_PASSWORD, SESSION_SECRET, and DB_PATH")
	}
	if !filepath.IsAbs(cfg.DBPath) {
		cfg.DBPath = filepath.Join(filepath.Dir(absEnv), cfg.DBPath)
	}
	return cfg, nil
}

func migrate(db *sql.DB) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS login_failures (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			ip TEXT NOT NULL,
			attempted_at INTEGER NOT NULL
		);`,
		`CREATE INDEX IF NOT EXISTS idx_login_failures_ip_time
			ON login_failures (ip, attempted_at);`,
		`CREATE TABLE IF NOT EXISTS members (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			first_name TEXT NOT NULL,
			last_name TEXT NOT NULL,
			role TEXT NOT NULL,
			city TEXT NOT NULL,
			state TEXT NOT NULL,
			handicap TEXT NOT NULL,
			home_course TEXT NOT NULL,
			guest_fee TEXT NOT NULL,
			bio TEXT NOT NULL,
			email TEXT NOT NULL,
			phone TEXT NOT NULL,
			imported_at INTEGER NOT NULL
		);`,
		`CREATE INDEX IF NOT EXISTS idx_members_state_city ON members (state, city);`,
		`CREATE INDEX IF NOT EXISTS idx_members_course ON members (home_course);`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", a.handleHome)
	mux.HandleFunc("GET /assets/app.css", a.handleCSS)
	mux.HandleFunc("GET /assets/app.js", a.handleJS)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))
	mux.HandleFunc("GET /login", a.handleLoginGet)
	mux.HandleFunc("POST /login", a.handleLoginPost)
	mux.HandleFunc("POST /logout", a.handleLogout)
	mux.HandleFunc("GET /admin", a.requireAuth(a.handleAdmin))
	mux.HandleFunc("POST /admin/upload", a.requireAuth(a.handleUpload))
	return secureHeaders(mux)
}

func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func (a *App) handleHome(w http.ResponseWriter, r *http.Request) {
	members, err := a.listMembers(r.Context())
	if err != nil {
		http.Error(w, "could not load directory", http.StatusInternalServerError)
		return
	}
	membersJSON, err := json.Marshal(members)
	if err != nil {
		http.Error(w, "could not render directory", http.StatusInternalServerError)
		return
	}
	a.render(w, "home", PageData{
		Title:     "Birdies & Biscuits",
		Members:   template.JS(membersJSON),
		Summary:   summarizeMembers(members),
		StartedAt: time.Now().Format(time.RFC3339),
	})
}

func (a *App) handleCSS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = io.WriteString(w, appCSS)
}

func (a *App) handleJS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = io.WriteString(w, appJS)
}

func (a *App) handleLoginGet(w http.ResponseWriter, r *http.Request) {
	if a.isAuthed(r) {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	a.render(w, "login", PageData{Title: "Admin Login"})
}

func (a *App) handleLoginPost(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	now := time.Now()
	blocked, err := a.isBlocked(r.Context(), ip, now)
	if err != nil {
		http.Error(w, "login unavailable", http.StatusInternalServerError)
		return
	}
	if blocked {
		http.Error(w, "too many login attempts", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	username := r.FormValue("username")
	password := r.FormValue("password")
	if subtleEqual(username, a.cfg.AdminUsername) && subtleEqual(password, a.cfg.AdminPassword) {
		sessionID, err := a.sessions.Create(now)
		if err != nil {
			http.Error(w, "could not create session", http.StatusInternalServerError)
			return
		}
		a.setSessionCookie(w, sessionID, now)
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	blocked, err = a.recordFailure(r.Context(), ip, now)
	if err != nil {
		http.Error(w, "login unavailable", http.StatusInternalServerError)
		return
	}
	if blocked {
		http.Error(w, "too many login attempts", http.StatusForbidden)
		return
	}
	w.WriteHeader(http.StatusUnauthorized)
	a.render(w, "login", PageData{Title: "Admin Login", Error: "Invalid username or password."})
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	if sessionID, ok := a.readSessionCookie(r); ok {
		a.sessions.Delete(sessionID)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (a *App) handleAdmin(w http.ResponseWriter, r *http.Request) {
	members, err := a.listMembers(r.Context())
	if err != nil {
		http.Error(w, "could not load admin", http.StatusInternalServerError)
		return
	}
	message := r.URL.Query().Get("message")
	errMsg := r.URL.Query().Get("error")
	a.render(w, "admin", PageData{
		Title:    "Admin",
		IsAuthed: true,
		Summary:  summarizeMembers(members),
		Message:  message,
		Error:    errMsg,
	})
}

func (a *App) handleUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxCSVBytes)
	if err := r.ParseMultipartForm(maxCSVBytes); err != nil {
		redirectAdmin(w, r, "", "Upload a CSV file under 5 MB.")
		return
	}
	file, header, err := r.FormFile("csv")
	if err != nil {
		redirectAdmin(w, r, "", "Choose a CSV file before uploading.")
		return
	}
	defer file.Close()
	if !strings.EqualFold(filepath.Ext(header.Filename), ".csv") {
		redirectAdmin(w, r, "", "The uploaded file must use the .csv extension.")
		return
	}
	count, err := a.importCSV(r.Context(), file)
	if err != nil {
		redirectAdmin(w, r, "", err.Error())
		return
	}
	redirectAdmin(w, r, fmt.Sprintf("Imported %d directory records.", count), "")
}

func redirectAdmin(w http.ResponseWriter, r *http.Request, message, errMsg string) {
	q := make([]string, 0, 2)
	if message != "" {
		q = append(q, "message="+urlQueryEscape(message))
	}
	if errMsg != "" {
		q = append(q, "error="+urlQueryEscape(errMsg))
	}
	target := "/admin"
	if len(q) > 0 {
		target += "?" + strings.Join(q, "&")
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func urlQueryEscape(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(template.URLQueryEscaper(value), "+", "%20"), "%2B", "+")
}

func (a *App) render(w http.ResponseWriter, name string, data PageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.tpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("render %s: %v", name, err)
	}
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
	sessionID, ok := a.readSessionCookie(r)
	if !ok {
		return false
	}
	return a.sessions.Valid(sessionID, time.Now())
}

func (a *App) setSessionCookie(w http.ResponseWriter, sessionID string, now time.Time) {
	signed := a.signSession(sessionID)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    signed,
		Path:     "/",
		Expires:  now.Add(sessionLifetime),
		MaxAge:   int(sessionLifetime.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func (a *App) readSessionCookie(r *http.Request) (string, bool) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return "", false
	}
	sessionID, signature, ok := strings.Cut(cookie.Value, ".")
	if !ok || sessionID == "" || signature == "" {
		return "", false
	}
	expected := a.sessionSignature(sessionID)
	if !hmac.Equal([]byte(signature), []byte(expected)) {
		return "", false
	}
	return sessionID, true
}

func (a *App) signSession(sessionID string) string {
	return sessionID + "." + a.sessionSignature(sessionID)
}

func (a *App) sessionSignature(sessionID string) string {
	mac := hmac.New(sha256.New, []byte(a.cfg.SessionSecret))
	mac.Write([]byte(sessionID))
	return hex.EncodeToString(mac.Sum(nil))
}

func NewSessionStore() *SessionStore {
	return &SessionStore{sessions: map[string]time.Time{}}
}

func (s *SessionStore) Create(now time.Time) (string, error) {
	id, err := randomToken(32)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeLocked(now)
	s.sessions[id] = now.Add(sessionLifetime)
	return id, nil
}

func (s *SessionStore) Valid(id string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeLocked(now)
	expiresAt, ok := s.sessions[id]
	return ok && expiresAt.After(now)
}

func (s *SessionStore) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
}

func (s *SessionStore) purgeLocked(now time.Time) {
	for id, expiresAt := range s.sessions {
		if !expiresAt.After(now) {
			delete(s.sessions, id)
		}
	}
}

func (a *App) isBlocked(ctx context.Context, ip string, now time.Time) (bool, error) {
	if err := a.purgeFailures(ctx, now); err != nil {
		return false, err
	}
	count, err := a.countFailures(ctx, ip, now)
	if err != nil {
		return false, err
	}
	return count >= maxLoginFailures, nil
}

func (a *App) recordFailure(ctx context.Context, ip string, now time.Time) (bool, error) {
	if err := a.purgeFailures(ctx, now); err != nil {
		return false, err
	}
	if _, err := a.db.ExecContext(ctx, `INSERT INTO login_failures (ip, attempted_at) VALUES (?, ?)`, ip, now.Unix()); err != nil {
		return false, err
	}
	count, err := a.countFailures(ctx, ip, now)
	if err != nil {
		return false, err
	}
	return count >= maxLoginFailures, nil
}

func (a *App) purgeFailures(ctx context.Context, now time.Time) error {
	_, err := a.db.ExecContext(ctx, `DELETE FROM login_failures WHERE attempted_at < ?`, now.Add(-loginWindow).Unix())
	return err
}

func (a *App) countFailures(ctx context.Context, ip string, now time.Time) (int, error) {
	var count int
	err := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM login_failures WHERE ip = ? AND attempted_at >= ?`, ip, now.Add(-loginWindow).Unix()).Scan(&count)
	return count, err
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (a *App) importCSV(ctx context.Context, file multipart.File) (int, error) {
	reader := csv.NewReader(file)
	reader.FieldsPerRecord = -1
	reader.TrimLeadingSpace = true
	headers, err := reader.Read()
	if err != nil {
		return 0, errors.New("The CSV must include a header row.")
	}
	index, err := mapHeaders(headers)
	if err != nil {
		return 0, err
	}
	var members []Member
	seenRows := map[string]int{}
	rowNum := 1
	importedAt := time.Now().Unix()
	for {
		row, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		rowNum++
		if err != nil {
			return 0, fmt.Errorf("CSV row %d could not be read: %w", rowNum, err)
		}
		if rowIsEmpty(row) {
			continue
		}
		member, err := memberFromRow(row, index, rowNum, importedAt)
		if err != nil {
			return 0, err
		}
		key := duplicateMemberKey(member)
		if firstRow, ok := seenRows[key]; ok {
			return 0, fmt.Errorf("Duplicate CSV row found for %s %s at rows %d and %d. Delete the duplicate row before uploading.", member.FirstName, member.LastName, firstRow, rowNum)
		}
		seenRows[key] = rowNum
		members = append(members, member)
	}
	if len(members) == 0 {
		return 0, errors.New("The CSV did not contain any member records.")
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM members`); err != nil {
		return 0, err
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO members
		(first_name, last_name, role, city, state, handicap, home_course, guest_fee, bio, email, phone, imported_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	for _, m := range members {
		if _, err := stmt.ExecContext(ctx, m.FirstName, m.LastName, m.Role, m.City, m.State, m.Handicap, m.HomeCourse, m.GuestFee, m.Bio, m.Email, m.Phone, m.ImportedAt); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(members), nil
}

func mapHeaders(headers []string) (map[string]int, error) {
	aliases := map[string][]string{
		"first_name":  {"first name", "firstname", "first_name"},
		"last_name":   {"last name", "lastname", "last_name"},
		"role":        {"role in cfa", "role in chick-fil-a", "role at chick-fil-a", "role in chickfila", "role at chickfila", "role"},
		"city":        {"city"},
		"state":       {"state"},
		"handicap":    {"handicap", "golf handicap"},
		"home_course": {"home course", "home golf course", "course", "club"},
		"guest_fee":   {"guest fee", "guest fees", "fee"},
		"bio":         {"bio", "biography"},
		"email":       {"email", "email address"},
		"phone":       {"phone", "phone number", "mobile"},
	}
	normalized := map[string]int{}
	for i, header := range headers {
		normalized[normalizeHeader(header)] = i
	}
	index := map[string]int{}
	for field, options := range aliases {
		found := -1
		for _, option := range options {
			if idx, ok := normalized[normalizeHeader(option)]; ok {
				found = idx
				break
			}
		}
		if found == -1 {
			return nil, fmt.Errorf("Missing required CSV column: %s.", strings.ReplaceAll(field, "_", " "))
		}
		index[field] = found
	}
	return index, nil
}

var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

func normalizeHeader(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "&", "and")
	return strings.Trim(nonAlnum.ReplaceAllString(value, " "), " ")
}

func memberFromRow(row []string, index map[string]int, rowNum int, importedAt int64) (Member, error) {
	get := func(key string) string {
		idx := index[key]
		if idx >= len(row) {
			return ""
		}
		return cleanCell(row[idx])
	}
	member := Member{
		FirstName:  get("first_name"),
		LastName:   get("last_name"),
		Role:       canonicalRole(get("role")),
		City:       get("city"),
		State:      strings.ToUpper(get("state")),
		Handicap:   get("handicap"),
		HomeCourse: get("home_course"),
		GuestFee:   get("guest_fee"),
		Bio:        get("bio"),
		Email:      strings.ToLower(get("email")),
		Phone:      get("phone"),
		ImportedAt: importedAt,
	}
	required := map[string]string{
		"first name":  member.FirstName,
		"last name":   member.LastName,
		"role":        member.Role,
		"city":        member.City,
		"state":       member.State,
		"handicap":    member.Handicap,
		"home course": member.HomeCourse,
		"guest fee":   member.GuestFee,
		"email":       member.Email,
		"phone":       member.Phone,
	}
	for field, value := range required {
		if value == "" {
			return Member{}, fmt.Errorf("Row %d is missing %s.", rowNum, field)
		}
	}
	if _, err := mail.ParseAddress(member.Email); err != nil {
		return Member{}, fmt.Errorf("Row %d has an invalid email address.", rowNum)
	}
	if !validPhone(member.Phone) {
		return Member{}, fmt.Errorf("Row %d has an invalid phone number.", rowNum)
	}
	return member, nil
}

func cleanCell(value string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
}

func canonicalRole(value string) string {
	switch normalizeHeader(value) {
	case "owner operator":
		return "Owner Operator"
	case "support center staff", "support center":
		return "Support Center Staff"
	case "operator spouse", "owner operator spouse", "spouse":
		return "Operator Spouse"
	default:
		return cleanCell(value)
	}
}

func validPhone(value string) bool {
	digits := 0
	for _, r := range value {
		if r >= '0' && r <= '9' {
			digits++
		}
	}
	return digits >= 7 && digits <= 15
}

func rowIsEmpty(row []string) bool {
	for _, value := range row {
		if strings.TrimSpace(value) != "" {
			return false
		}
	}
	return true
}

func duplicateMemberKey(member Member) string {
	parts := []string{
		member.FirstName,
		member.LastName,
		member.Email,
		member.Phone,
		member.HomeCourse,
		member.City,
		member.State,
	}
	for i, part := range parts {
		parts[i] = normalizeHeader(part)
	}
	return strings.Join(parts, "|")
}

func (a *App) listMembers(ctx context.Context) ([]Member, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT id, first_name, last_name, role, city, state, handicap, home_course, guest_fee, bio, email, phone, imported_at
		FROM members ORDER BY state, city, home_course, last_name, first_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	members := []Member{}
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.ID, &m.FirstName, &m.LastName, &m.Role, &m.City, &m.State, &m.Handicap, &m.HomeCourse, &m.GuestFee, &m.Bio, &m.Email, &m.Phone, &m.ImportedAt); err != nil {
			return nil, err
		}
		members = append(members, m)
	}
	return members, rows.Err()
}

func summarizeMembers(members []Member) ImportSummary {
	summary := ImportSummary{Count: len(members)}
	for _, member := range members {
		if member.ImportedAt > summary.ImportedAt {
			summary.ImportedAt = member.ImportedAt
		}
	}
	return summary
}

func randomToken(bytes int) (string, error) {
	buf := make([]byte, bytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func subtleEqual(a, b string) bool {
	ha := sha256.Sum256([]byte(a))
	hb := sha256.Sum256([]byte(b))
	return hmac.Equal(ha[:], hb[:])
}

var pageTemplates = `{{define "layoutTop"}}<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>{{.Title}}</title>
  <link rel="stylesheet" href="/assets/app.css">
</head>
<body>
{{end}}
{{define "layoutBottom"}}</body></html>{{end}}

{{define "home"}}
{{template "layoutTop" .}}
<main class="public-shell" data-members='{{.Members}}'>
  <canvas class="ambient-gl" id="ambientGl" aria-hidden="true"></canvas>
  <header class="app-topbar">
    <a class="topbar-brand" href="/" aria-label="Birdies & Biscuits home">
      <img src="/static/logo.png" alt="">
      <span>Birdies & Biscuits Directory</span>
    </a>
  </header>
  <div class="workspace">
  <section class="intro-panel step is-active" data-step="intro">
    <div class="brand-row">
      <span class="module-kicker">Member Connection Finder</span>
    </div>
    <div class="hero-copy">
      <p class="eyebrow">Birdies & Biscuits</p>
      <h1>Find the right Chick-fil-A golf connection before you travel.</h1>
      <p class="lede">Choose a destination, compare course access, and reach the operator or support-center contact who can help.</p>
    </div>
    <div class="quick-stats" aria-label="Directory summary">
      <div><strong id="stat-members">{{.Summary.Count}}</strong><span>contacts</span></div>
      <div><strong id="stat-states">0</strong><span>states</span></div>
      <div><strong id="stat-courses">0</strong><span>course notes</span></div>
    </div>
    <button class="primary-action" type="button" data-next="search">Start exploring</button>
  </section>

  <section class="finder-panel step" data-step="search">
    <div class="panel-head">
      <button class="ghost-action" type="button" data-next="intro">Back</button>
      <span>Step 1 of 4</span>
    </div>
    <h2>How would you like to explore?</h2>
    <p class="muted">Start with a destination state, or find a specific operator first.</p>
    <div class="path-grid">
      <button class="choice-card path-card" type="button" data-next="state">
        <img src="/static/location.png" alt="">
        <strong>Search by state</strong>
        <span>Choose where you are traveling, then compare course notes or operators there.</span>
      </button>
      <button class="choice-card path-card" type="button" data-next="operator">
        <img src="/static/member.png" alt="">
        <strong>Search by operator</strong>
        <span>Find a person across the full directory, then review their course access.</span>
      </button>
    </div>
  </section>

  <section class="finder-panel step" data-step="state">
    <div class="panel-head">
      <button class="ghost-action" type="button" data-next="search">Back</button>
      <span>State search</span>
    </div>
    <h2>Where are you headed?</h2>
    <p class="muted">Start typing to find a state, then choose where you are traveling.</p>
    <div class="filter-row state-search">
      <input id="stateSearchBox" type="search" placeholder="Search states, like Georgia or South Carolina" autocomplete="off">
    </div>
    <div class="state-grid" id="stateGrid"></div>
  </section>

  <section class="finder-panel step" data-step="operator">
    <div class="panel-head">
      <button class="ghost-action" type="button" data-next="search">Back</button>
      <span>Operator search</span>
    </div>
    <h2>Who are you looking for?</h2>
    <p class="muted">Search the full directory by operator name, state, city, course, role, or handicap.</p>
    <div class="filter-row state-search">
      <input id="operatorSearchBox" type="search" placeholder="Search operators, cities, states, or courses" autocomplete="off">
    </div>
    <div class="course-list" id="operatorList"></div>
  </section>

  <section class="finder-panel step" data-step="path">
    <div class="panel-head">
      <button class="ghost-action" type="button" data-next="state">Back</button>
      <span>Step 2 of 4</span>
    </div>
    <h2 id="pathTitle">How would you like to search?</h2>
    <p class="muted">Choose the path that matches how you think about the trip.</p>
    <div class="path-grid">
      <button class="choice-card path-card" type="button" data-mode="course">
        <img src="/static/golf_1.png" alt="">
        <strong>Choose a golf course</strong>
        <span>Compare course notes first, then see matching contacts.</span>
      </button>
      <button class="choice-card path-card" type="button" data-mode="operator">
        <img src="/static/member.png" alt="">
        <strong>Choose an operator</strong>
        <span>Find a person first, then review their course access.</span>
      </button>
    </div>
  </section>

  <section class="finder-panel step" data-step="course">
    <div class="panel-head">
      <button class="ghost-action" type="button" data-next="path">Back</button>
      <span id="courseStepLabel">Step 3 of 4</span>
    </div>
    <h2 id="courseTitle">Select a course note.</h2>
    <p class="muted" id="courseHelp">Some entries name a specific club, while others describe public options in an area.</p>
    <div class="course-list" id="courseList"></div>
  </section>

  <section class="results-panel step" data-step="results">
    <div class="results-top">
      <div class="results-heading">
        <button class="ghost-action" type="button" id="resultsBackButton" data-next="course">Back</button>
        <div>
          <p class="eyebrow" id="resultsEyebrow">Matches</p>
          <h2 id="resultsTitle">Available connections</h2>
        </div>
      </div>
      <button class="secondary-action" type="button" data-next="state">New search</button>
    </div>
    <div class="results-meta" id="resultsMeta" aria-label="Selected search context"></div>
    <div class="filter-row">
      <input id="searchBox" type="search" placeholder="Filter by name, city, role, course, or handicap">
    </div>
    <div class="member-grid" id="memberGrid"></div>
  </section>
  </div>
</main>
<script src="/assets/app.js"></script>
{{template "layoutBottom" .}}
{{end}}

{{define "login"}}
{{template "layoutTop" .}}
<main class="auth-page">
  <section class="auth-card">
    <div class="loader-line" aria-hidden="true"></div>
    <img class="auth-logo" src="/static/logo.png" alt="Birdies & Biscuits">
    <p class="eyebrow">Admin Portal</p>
    <h1>Sign in to update the directory.</h1>
    {{if .Error}}<div class="alert error">{{.Error}}</div>{{end}}
    <form method="post" action="/login" class="stack-form">
      <label>Username<input name="username" type="text" autocomplete="username" required autofocus></label>
      <label>Password<input name="password" type="password" autocomplete="current-password" required></label>
      <button class="primary-action full" type="submit">Sign in</button>
    </form>
    <a class="quiet-link" href="/">Return to public directory</a>
  </section>
</main>
{{template "layoutBottom" .}}
{{end}}

{{define "admin"}}
{{template "layoutTop" .}}
<main class="admin-shell">
  <nav class="admin-nav">
    <a href="/" class="mark-link"><span class="mark"><img src="/static/logo.png" alt=""></span><span>Public directory</span></a>
    <form method="post" action="/logout"><button class="ghost-action" type="submit">Logout</button></form>
  </nav>
  <section class="admin-hero">
    <div>
      <p class="eyebrow">Admin Portal</p>
      <h1>Upload the latest member CSV.</h1>
      <p class="lede">A successful upload replaces the current public directory with the rows in the new spreadsheet.</p>
    </div>
    <div class="admin-stat"><img src="/static/member.png" alt=""><strong>{{.Summary.Count}}</strong><span>active records</span></div>
  </section>
  {{if .Message}}<div class="alert success">{{.Message}}</div>{{end}}
  {{if .Error}}<div class="alert error">{{.Error}}</div>{{end}}
  <section class="upload-zone">
    <form method="post" action="/admin/upload" enctype="multipart/form-data" class="upload-form">
      <label class="file-picker">
        <input name="csv" type="file" accept=".csv,text/csv" required>
        <span>Choose CSV</span>
        <strong id="fileName">No file selected</strong>
      </label>
      <button class="primary-action" type="submit">Import directory</button>
    </form>
    <div class="schema-box">
      <img src="/static/paper.png" alt="">
      <h2>Expected columns</h2>
      <p>FIRST NAME, LAST NAME, ROLE IN CFA, CITY, STATE, HANDICAP, HOME COURSE, GUEST FEE, BIO, EMAIL, PHONE.</p>
      <p>Email and phone are validated. Course, guest fee, handicap, and bio can be descriptive text.</p>
    </div>
  </section>
</main>
<script>
document.querySelector('input[type=file]')?.addEventListener('change', function () {
  document.getElementById('fileName').textContent = this.files[0]?.name || 'No file selected';
});
</script>
{{template "layoutBottom" .}}
{{end}}`

var appCSS = `
:root {
  --ink: #1f2933;
  --muted: #66717d;
  --line: #d9dee5;
  --soft: #f3f5f7;
  --paper: #ffffff;
  --red: #df002b;
  --red-dark: #b90024;
  --red-soft: #fff0f3;
  --navy: #102742;
  --shadow: 0 14px 28px rgba(17, 24, 39, 0.14);
}
* { box-sizing: border-box; }
body {
  margin: 0;
  color: var(--ink);
  background: #e9ecef;
  font-family: Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
  min-height: 100vh;
}
.ambient-gl {
  position: fixed;
  inset: 86px 0 0 0;
  width: 100vw;
  height: calc(100vh - 86px);
  opacity: .34;
  pointer-events: none;
  z-index: 0;
}
button, input { font: inherit; }
button { cursor: pointer; }
.public-shell {
  min-height: 100vh;
  padding: 104px clamp(18px, 3vw, 42px) 36px;
}
.app-topbar {
  position: fixed;
  top: 0;
  left: 0;
  right: 0;
  height: 86px;
  background: #fff;
  border-bottom: 1px solid #cfd5dc;
  box-shadow: 0 3px 12px rgba(17,24,39,.14);
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 18px;
  padding: 0 clamp(18px, 4vw, 58px);
  z-index: 4;
}
.topbar-brand {
  display: flex;
  align-items: center;
  gap: 18px;
  color: #343e49;
  font-size: clamp(1.05rem, 2vw, 1.7rem);
  font-weight: 900;
  text-decoration: none;
}
.topbar-brand img {
  width: 58px;
  height: 58px;
  object-fit: contain;
}
.workspace {
  position: relative;
  z-index: 1;
  width: 100%;
}
.step {
  display: none;
}
.step.is-active {
  display: block;
}
.intro-panel, .finder-panel, .results-panel, .auth-card, .upload-zone {
  background: var(--paper);
  border: 1px solid #dde2e8;
  box-shadow: var(--shadow);
}
.intro-panel {
  position: relative;
  overflow: hidden;
  width: 100%;
  min-height: 650px;
  padding: clamp(28px, 5vw, 58px);
  border-radius: 8px;
}
.brand-row, .admin-nav, .panel-head, .results-top, .filter-row, .mark-link {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
}
.brand-row {
  min-height: 40px;
  align-items: flex-start;
}
.mark {
  display: inline-grid;
  place-items: center;
  width: 48px;
  height: 48px;
  border-radius: 8px;
  background: #fff;
  overflow: hidden;
  border: 1px solid var(--line);
}
.mark img {
  width: 64px;
  height: 64px;
  object-fit: cover;
}
.module-kicker {
  color: var(--navy);
  font-weight: 900;
  font-size: clamp(1rem, 2vw, 1.35rem);
}
.quiet-link, .mark-link {
  color: var(--red);
  text-decoration: none;
  font-weight: 900;
}
.hero-copy { max-width: 780px; padding: 32px 0 28px; position: relative; z-index: 1; }
.eyebrow {
  margin: 0 0 10px;
  color: var(--red);
  font-size: .78rem;
  font-weight: 800;
  letter-spacing: 0;
  text-transform: uppercase;
}
h1, h2, p { margin-top: 0; }
h1 {
  margin-bottom: 18px;
  font-size: clamp(2.35rem, 5.8vw, 5.15rem);
  line-height: 1;
  letter-spacing: 0;
  color: #303a45;
}
h2 {
  margin-bottom: 10px;
  font-size: clamp(1.7rem, 3vw, 2.65rem);
  line-height: 1.05;
  letter-spacing: 0;
  color: #303a45;
}
.lede {
  color: var(--muted);
  font-size: clamp(1.05rem, 2vw, 1.25rem);
  line-height: 1.55;
  max-width: 700px;
}
.muted { color: var(--muted); line-height: 1.55; }
.quick-stats {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  border-top: 1px solid var(--line);
  border-bottom: 1px solid var(--line);
  margin: 14px 0 28px;
  max-width: 760px;
}
.quick-stats div {
  padding: 18px 14px;
  border-right: 1px solid var(--line);
}
.quick-stats div:last-child { border-right: 0; }
.quick-stats strong {
  display: block;
  font-size: 1.85rem;
}
.quick-stats span { color: var(--muted); }
.primary-action, .secondary-action, .ghost-action {
  min-height: 44px;
  border-radius: 8px;
  border: 1px solid transparent;
  padding: 0 18px;
  font-weight: 800;
}
.primary-action {
  background: var(--red);
  color: white;
  box-shadow: 0 10px 22px rgba(223,0,43,.22);
}
.secondary-action {
  background: var(--red-soft);
  color: var(--red);
}
.ghost-action {
  background: transparent;
  border-color: var(--line);
  color: var(--red);
}
.finder-panel, .results-panel {
  width: 100%;
  border-radius: 8px;
  padding: clamp(20px, 4vw, 42px);
}
.results-panel {
  background: linear-gradient(180deg, #ffffff 0%, #fafbfc 100%);
}
.panel-head {
  color: var(--muted);
  margin-bottom: 32px;
  font-weight: 700;
}
.results-top {
  align-items: flex-start;
  border-bottom: 1px solid var(--line);
  padding-bottom: 20px;
  margin-bottom: 18px;
}
.results-heading {
  display: flex;
  align-items: flex-start;
  gap: 18px;
  min-width: 0;
}
.results-heading .ghost-action {
  flex: 0 0 auto;
  margin-top: 2px;
}
.results-heading h2 {
  margin-bottom: 0;
  overflow-wrap: anywhere;
}
.results-meta {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  margin: 0 0 18px;
}
.meta-chip {
  display: inline-flex;
  align-items: center;
  min-height: 32px;
  border-radius: 8px;
  border: 1px solid #dde2e8;
  background: #fff;
  color: var(--muted);
  padding: 0 11px;
  font-size: .86rem;
  font-weight: 800;
}
.meta-chip strong {
  color: var(--navy);
  margin-left: 5px;
}
.state-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(178px, 1fr));
  gap: 12px;
  margin-top: 26px;
}
.choice-card {
  width: 100%;
  min-height: 96px;
  text-align: left;
  background: var(--paper);
  border: 1px solid var(--line);
  border-radius: 8px;
  padding: 16px;
  transition: transform .18s ease, border-color .18s ease, box-shadow .18s ease;
}
.choice-card:hover {
  transform: translateY(-2px);
  border-color: rgba(223,0,43,.45);
  box-shadow: 0 14px 34px rgba(17,24,39,.12);
}
.choice-card strong {
  display: block;
  font-size: 1.25rem;
  color: var(--navy);
  overflow-wrap: anywhere;
  line-height: 1.12;
}
.choice-card span {
  display: block;
  margin-top: 8px;
  color: var(--muted);
  font-size: .92rem;
  overflow-wrap: anywhere;
}
.state-search {
  margin-top: 22px;
  max-width: 560px;
}
.path-grid {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 16px;
  margin-top: 28px;
}
.path-card {
  min-height: 210px;
  display: grid;
  align-content: start;
  gap: 8px;
}
.path-card img {
  width: 76px;
  height: 76px;
  object-fit: cover;
  border: 1px solid var(--line);
  border-radius: 8px;
  margin-bottom: 12px;
}
.path-card strong {
  font-size: 1.45rem;
}
.course-list {
  display: grid;
  gap: 12px;
  margin-top: 26px;
}
.course-card {
  display: grid;
  grid-template-columns: 1fr auto;
  gap: 16px;
  align-items: center;
}
.course-card strong {
  font-size: 1.02rem;
}
.filter-row input {
  width: 100%;
  min-height: 48px;
  border: 1px solid var(--line);
  border-radius: 8px;
  padding: 0 16px;
  background: #fff;
}
.member-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(280px, 1fr));
  gap: 16px;
  margin-top: 20px;
}
.member-card {
  background: #fff;
  border: 1px solid var(--line);
  border-radius: 8px;
  padding: 20px;
  box-shadow: 0 8px 22px rgba(17,24,39,.08);
  display: flex;
  flex-direction: column;
  min-height: 100%;
}
.member-card-top {
  display: flex;
  justify-content: space-between;
  gap: 14px;
  padding-bottom: 14px;
  border-bottom: 1px solid var(--line);
}
.member-card h3 {
  margin: 0 0 4px;
  font-size: 1.22rem;
  color: var(--navy);
}
.member-card .muted {
  margin-bottom: 0;
}
.pill-row {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  margin: 14px 0 4px;
}
.pill {
  display: inline-flex;
  align-items: center;
  min-height: 28px;
  border-radius: 999px;
  background: var(--red-soft);
  border: 1px solid #ffd1da;
  color: var(--red-dark);
  padding: 0 10px;
  font-size: .86rem;
  font-weight: 700;
}
.detail-block {
  margin: 16px 0 0;
}
.detail-block strong {
  display: block;
  margin-bottom: 5px;
  color: var(--red);
  font-size: .78rem;
  letter-spacing: 0;
  text-transform: uppercase;
}
.detail-block p {
  margin-bottom: 0;
  line-height: 1.48;
}
.contact-row {
  display: flex;
  flex-wrap: wrap;
  gap: 10px;
  margin-top: auto;
  padding-top: 18px;
}
.contact-row a {
  display: inline-flex;
  align-items: center;
  min-height: 38px;
  border-radius: 8px;
  border: 1px solid #ffd1da;
  background: var(--red-soft);
  color: var(--red-dark);
  padding: 0 12px;
  font-weight: 800;
  text-decoration: none;
}
.empty-state {
  padding: 24px;
  border: 1px dashed var(--line);
  border-radius: 8px;
  color: var(--muted);
  background: rgba(255,255,255,.7);
}
.auth-page {
  min-height: 100vh;
  display: grid;
  place-items: center;
  width: min(100% - 32px, 1120px);
  margin: 0 auto;
}
.auth-card {
  width: min(430px, 100%);
  border-radius: 8px;
  padding: 32px;
  position: relative;
  overflow: hidden;
}
.auth-card h1 { font-size: 2rem; line-height: 1.08; }
.auth-logo {
  width: 96px;
  height: 96px;
  border-radius: 8px;
  object-fit: cover;
  border: 1px solid var(--line);
  margin-bottom: 18px;
}
.loader-line {
  position: absolute;
  top: 0;
  left: 0;
  height: 4px;
  width: 100%;
  background: linear-gradient(90deg, var(--red), var(--navy), var(--red));
  animation: loadbar .9s ease-out both;
}
@keyframes loadbar { from { transform: translateX(-100%); } to { transform: translateX(0); } }
.stack-form {
  display: grid;
  gap: 14px;
  margin-top: 22px;
}
label {
  display: grid;
  gap: 7px;
  color: var(--muted);
  font-weight: 800;
  font-size: .92rem;
}
input[type="text"], input[type="password"], input[type="file"], input[type="search"] {
  color: var(--ink);
}
label input:not([type="file"]) {
  width: 100%;
  min-height: 46px;
  border: 1px solid var(--line);
  border-radius: 8px;
  padding: 0 12px;
  background: #fff;
}
.full { width: 100%; }
.quiet-link {
  display: inline-block;
  margin-top: 18px;
}
.alert {
  margin: 18px 0;
  border-radius: 8px;
  padding: 12px 14px;
  font-weight: 700;
}
.alert.error {
  background: #fff1f1;
  color: var(--red-dark);
  border: 1px solid #f1caca;
}
.alert.success {
  background: #f2fbf5;
  color: #166534;
  border: 1px solid #cbeed6;
}
.admin-shell {
  width: min(1120px, calc(100% - 32px));
  margin: 0 auto;
  padding: 28px 0 54px;
}
.admin-nav {
  margin-bottom: 38px;
}
.mark-link {
  justify-content: flex-start;
  font-weight: 800;
}
.admin-hero {
  display: grid;
  grid-template-columns: minmax(0, 1fr) 180px;
  gap: 24px;
  align-items: end;
  margin-bottom: 22px;
}
.admin-hero h1 {
  max-width: 760px;
  font-size: clamp(2.2rem, 5vw, 4.5rem);
}
.admin-stat {
  background: var(--red);
  color: #fff;
  border-radius: 8px;
  padding: 22px;
}
.admin-stat img {
  width: 62px;
  height: 62px;
  border-radius: 8px;
  object-fit: cover;
  background: #fff;
  margin-bottom: 14px;
}
.admin-stat strong {
  display: block;
  font-size: 2.4rem;
}
.upload-zone {
  display: grid;
  grid-template-columns: minmax(0, 1.2fr) minmax(260px, .8fr);
  gap: 20px;
  border-radius: 8px;
  padding: 24px;
}
.upload-form {
  display: grid;
  align-content: start;
  gap: 16px;
}
.file-picker {
  min-height: 160px;
  border: 1px dashed rgba(223,0,43,.4);
  border-radius: 8px;
  place-items: center;
  text-align: center;
  background: var(--soft);
  padding: 20px;
}
.file-picker input { max-width: 240px; }
.file-picker span {
  color: var(--red);
  font-size: 1.35rem;
}
.file-picker strong {
  color: var(--muted);
  overflow-wrap: anywhere;
}
.schema-box {
  border-left: 1px solid var(--line);
  padding-left: 20px;
}
.schema-box img {
  width: 74px;
  height: 74px;
  object-fit: cover;
  border-radius: 8px;
  border: 1px solid var(--line);
  margin-bottom: 12px;
}
.schema-box h2 {
  font-size: 1.35rem;
}
@media (max-width: 760px) {
  .ambient-gl {
    inset: 76px 0 0 0;
    width: 100vw;
    height: calc(100vh - 76px);
    opacity: .26;
  }
  .public-shell {
    padding: 92px 10px 24px;
  }
  .app-topbar {
    height: 76px;
    padding: 0 14px;
  }
  .topbar-brand { gap: 10px; }
  .topbar-brand img { width: 48px; height: 48px; }
  .topbar-brand span { font-size: 1rem; line-height: 1.1; }
  .intro-panel { min-height: 0; }
  .brand-row {
    min-height: 0;
    align-items: center;
  }
  .module-kicker { padding-top: 0; max-width: 190px; }
  .auth-page, .admin-shell { width: min(100% - 20px, 1120px); }
  .quick-stats, .admin-hero, .upload-zone, .path-grid { grid-template-columns: 1fr; }
  .quick-stats div { border-right: 0; border-bottom: 1px solid var(--line); }
  .quick-stats div:last-child { border-bottom: 0; }
  .course-card, .results-top { grid-template-columns: 1fr; align-items: start; }
  .results-top { display: grid; }
  .results-heading { display: grid; gap: 14px; }
  .results-heading .ghost-action { width: max-content; }
  .member-card-top { display: grid; }
  .brand-row, .admin-nav { align-items: flex-start; }
  .schema-box { border-left: 0; border-top: 1px solid var(--line); padding-left: 0; padding-top: 18px; }
  h1 { font-size: 2.5rem; }
}
`

var appJS = `
(function () {
  const root = document.querySelector('.public-shell');
  if (!root) return;

  const members = JSON.parse(root.dataset.members || '[]');
  let selectedState = '';
  let selectedMode = 'course';
  let selectedOption = '';
  let activeMatches = [];

  const stateGrid = document.getElementById('stateGrid');
  const stateSearchBox = document.getElementById('stateSearchBox');
  const operatorSearchBox = document.getElementById('operatorSearchBox');
  const operatorList = document.getElementById('operatorList');
  const courseList = document.getElementById('courseList');
  const memberGrid = document.getElementById('memberGrid');
  const searchBox = document.getElementById('searchBox');
  const resultsBackButton = document.getElementById('resultsBackButton');
  const unique = (items) => Array.from(new Set(items.filter(Boolean))).sort((a, b) => a.localeCompare(b));
  const byState = unique(members.map((m) => m.state));
  const byCourse = unique(members.map((m) => m.homeCourse));
  document.getElementById('stat-states').textContent = byState.length;
  document.getElementById('stat-courses').textContent = byCourse.length;

  document.addEventListener('click', function (event) {
    const next = event.target.closest('[data-next]');
    if (!next) return;
    showStep(next.dataset.next);
  });

  function showStep(step) {
    document.querySelectorAll('.step').forEach((node) => node.classList.remove('is-active'));
    document.querySelector('[data-step="' + step + '"]')?.classList.add('is-active');
    if (step === 'state') {
      if (stateSearchBox) stateSearchBox.value = '';
      renderStates('');
      window.setTimeout(() => stateSearchBox?.focus(), 40);
    }
    if (step === 'operator') {
      if (operatorSearchBox) operatorSearchBox.value = '';
      renderOperators('');
      window.setTimeout(() => operatorSearchBox?.focus(), 40);
    }
    if (step === 'results') {
      window.setTimeout(() => searchBox?.focus(), 40);
    }
  }

  function renderStates(query) {
    if (!members.length) {
      stateGrid.innerHTML = '<div class="empty-state">No directory data has been imported yet.</div>';
      return;
    }
    const normalizedQuery = String(query || '').trim().toLowerCase();
    const states = normalizedQuery ? byState.filter((state) => state.toLowerCase().includes(normalizedQuery)) : byState;
    if (!states.length) {
      stateGrid.innerHTML = '<div class="empty-state">No states match that search.</div>';
      return;
    }
    stateGrid.innerHTML = states.map((state) => {
      const count = members.filter((m) => m.state === state).length;
      const cities = unique(members.filter((m) => m.state === state).map((m) => m.city)).slice(0, 3).join(', ');
      return '<button class="choice-card" type="button" data-state="' + escapeHTML(state) + '">' +
        '<strong>' + escapeHTML(state) + '</strong><span>' + count + ' contact' + (count === 1 ? '' : 's') + '</span><span>' + escapeHTML(cities) + '</span></button>';
    }).join('');
  }

  stateSearchBox?.addEventListener('input', function () {
    renderStates(stateSearchBox.value);
  });

  operatorSearchBox?.addEventListener('input', function () {
    renderOperators(operatorSearchBox.value);
  });

  operatorList.addEventListener('click', function (event) {
    const card = event.target.closest('[data-operator]');
    if (!card) return;
    selectedState = '';
    selectedMode = 'operator';
    selectedOption = card.dataset.operator;
    activeMatches = members.filter((m) => memberName(m) === selectedOption);
    showResults('operator');
  });

  stateGrid.addEventListener('click', function (event) {
    const card = event.target.closest('[data-state]');
    if (!card) return;
    selectedState = card.dataset.state;
    selectedOption = '';
    document.getElementById('pathTitle').textContent = 'Search ' + selectedState + ' by course or operator.';
    showStep('path');
  });

  document.querySelectorAll('[data-mode]').forEach((button) => {
    button.addEventListener('click', function () {
      selectedMode = button.dataset.mode;
      selectedOption = '';
      renderOptions();
      showStep('course');
    });
  });

  function renderOptions() {
    const stateMembers = members.filter((m) => m.state === selectedState);
    if (selectedMode === 'operator') {
      document.getElementById('courseTitle').textContent = 'Choose a ' + selectedState + ' operator.';
      document.getElementById('courseHelp').textContent = 'Pick a person to see their role, course access, guest fee, and contact details.';
      const operators = unique(stateMembers.map((m) => memberName(m)));
      courseList.innerHTML = operators.map((name) => {
        const operatorMembers = stateMembers.filter((m) => memberName(m) === name);
        const cities = unique(operatorMembers.map((m) => m.city)).join(', ');
        const courses = unique(operatorMembers.map((m) => m.homeCourse)).length;
        return '<button class="choice-card course-card" type="button" data-option="' + escapeHTML(name) + '">' +
          '<span><strong>' + escapeHTML(name) + '</strong><span>' + escapeHTML(cities) + '</span></span>' +
          '<span>' + courses + ' course note' + (courses === 1 ? '' : 's') + '</span></button>';
      }).join('');
      return;
    }
    document.getElementById('courseTitle').textContent = 'Choose a ' + selectedState + ' course note.';
    document.getElementById('courseHelp').textContent = 'Some entries name a specific club, while others describe public options in an area.';
    const courses = unique(stateMembers.map((m) => m.homeCourse));
    courseList.innerHTML = courses.map((course) => {
      const courseMembers = stateMembers.filter((m) => m.homeCourse === course);
      const cities = unique(courseMembers.map((m) => m.city)).join(', ');
      return '<button class="choice-card course-card" type="button" data-option="' + escapeHTML(course) + '">' +
        '<span><strong>' + escapeHTML(course) + '</strong><span>' + escapeHTML(cities) + '</span></span>' +
        '<span>' + courseMembers.length + ' match' + (courseMembers.length === 1 ? '' : 'es') + '</span></button>';
    }).join('');
  }

  courseList.addEventListener('click', function (event) {
    const card = event.target.closest('[data-option]');
    if (!card) return;
    selectedOption = card.dataset.option;
    activeMatches = members.filter((m) => {
      if (m.state !== selectedState) return false;
      return selectedMode === 'operator' ? memberName(m) === selectedOption : m.homeCourse === selectedOption;
    });
    showResults('course');
  });

  searchBox.addEventListener('input', function () {
    const query = searchBox.value.trim().toLowerCase();
    if (!query) {
      renderMembers(activeMatches);
      return;
    }
    renderMembers(activeMatches.filter((m) => searchable(m).includes(query)));
  });

  function searchable(m) {
    return [m.firstName, m.lastName, m.role, m.city, m.state, m.handicap, m.homeCourse, m.guestFee, m.bio, m.email, m.phone].join(' ').toLowerCase();
  }

  function memberName(m) {
    return m.firstName + ' ' + m.lastName;
  }

  function renderOperators(query) {
    if (!members.length) {
      operatorList.innerHTML = '<div class="empty-state">No directory data has been imported yet.</div>';
      return;
    }
    const normalizedQuery = String(query || '').trim().toLowerCase();
    const operators = unique(members.map((m) => memberName(m)));
    const filtered = operators.filter((name) => {
      const operatorMembers = members.filter((m) => memberName(m) === name);
      return !normalizedQuery || operatorMembers.some((m) => searchable(m).includes(normalizedQuery) || name.toLowerCase().includes(normalizedQuery));
    });
    if (!filtered.length) {
      operatorList.innerHTML = '<div class="empty-state">No operators match that search.</div>';
      return;
    }
    operatorList.innerHTML = filtered.map((name) => {
      const operatorMembers = members.filter((m) => memberName(m) === name);
      const states = unique(operatorMembers.map((m) => m.state)).join(', ');
      const courses = unique(operatorMembers.map((m) => m.homeCourse)).length;
      const roles = unique(operatorMembers.map((m) => m.role)).join(', ');
      return '<button class="choice-card course-card" type="button" data-operator="' + escapeHTML(name) + '">' +
        '<span><strong>' + escapeHTML(name) + '</strong><span>' + escapeHTML(states + (roles ? ' / ' + roles : '')) + '</span></span>' +
        '<span>' + courses + ' course note' + (courses === 1 ? '' : 's') + '</span></button>';
    }).join('');
  }

  function showResults(backStep) {
    searchBox.value = '';
    renderMembers(activeMatches);
    const matchLabel = activeMatches.length + ' match' + (activeMatches.length === 1 ? '' : 'es');
    const stateLabel = selectedState || unique(activeMatches.map((m) => m.state)).join(', ');
    resultsBackButton.dataset.next = backStep;
    document.getElementById('resultsEyebrow').textContent = (stateLabel || 'Directory') + ' / ' + matchLabel;
    document.getElementById('resultsTitle').textContent = selectedOption;
    document.getElementById('resultsMeta').innerHTML =
      (stateLabel ? '<span class="meta-chip">State <strong>' + escapeHTML(stateLabel) + '</strong></span>' : '') +
      '<span class="meta-chip">Search by <strong>' + escapeHTML(selectedMode === 'operator' ? 'Operator' : 'Course') + '</strong></span>' +
      '<span class="meta-chip">Matches <strong>' + activeMatches.length + '</strong></span>';
    showStep('results');
  }

  function renderMembers(list) {
    if (!list.length) {
      memberGrid.innerHTML = '<div class="empty-state">No contacts match that filter.</div>';
      return;
    }
    memberGrid.innerHTML = list.map((m) => {
      const name = memberName(m);
      return '<article class="member-card">' +
        '<div class="member-card-top"><div><h3>' + escapeHTML(name) + '</h3>' +
        '<p class="muted">' + escapeHTML(m.city + ', ' + m.state) + '</p></div>' +
        '<span class="pill">' + escapeHTML(m.role) + '</span></div>' +
        '<div class="pill-row"><span class="pill">Handicap ' + escapeHTML(m.handicap) + '</span></div>' +
        '<div class="detail-block"><strong>Course access</strong><p>' + escapeHTML(m.homeCourse) + '</p></div>' +
        '<div class="detail-block"><strong>Guest fee</strong><p>' + escapeHTML(m.guestFee) + '</p></div>' +
        (m.bio ? '<div class="detail-block"><strong>Bio</strong><p>' + escapeHTML(m.bio) + '</p></div>' : '') +
        '<div class="contact-row"><a href="mailto:' + encodeURIComponent(m.email) + '">Email</a><a href="tel:' + escapeHTML(phoneHref(m.phone)) + '">' + escapeHTML(m.phone) + '</a></div>' +
      '</article>';
    }).join('');
  }

  function phoneHref(phone) {
    const digits = phone.replace(/[^0-9+]/g, '');
    return digits;
  }

  function initAmbientGL() {
    const canvas = document.getElementById('ambientGl');
    if (!canvas) return;
    const gl = canvas.getContext('webgl', { antialias: false, alpha: true });
    if (!gl) return;

    const vertexSource = 'attribute vec2 a;void main(){gl_Position=vec4(a,0.0,1.0);}';
    const fragmentSource = 'precision mediump float;uniform vec2 r;uniform float t;void main(){vec2 uv=gl_FragCoord.xy/r;float wave=sin((uv.x*7.0+t*.35)+sin(uv.y*5.0+t*.22))*0.5+0.5;float line=smoothstep(.955,1.0,wave)*(1.0-smoothstep(.0,.82,uv.y));float glow=smoothstep(.78,.0,distance(uv,vec2(.78,.28+sin(t*.18)*.04)))*.18;vec3 red=vec3(.88,.0,.16);vec3 navy=vec3(.06,.15,.26);float alpha=(line*.10+glow*.18);gl_FragColor=vec4(mix(navy,red,.72),alpha);}';
    const program = gl.createProgram();
    const vertex = compile(gl.VERTEX_SHADER, vertexSource);
    const fragment = compile(gl.FRAGMENT_SHADER, fragmentSource);
    if (!vertex || !fragment || !program) return;
    gl.attachShader(program, vertex);
    gl.attachShader(program, fragment);
    gl.linkProgram(program);
    if (!gl.getProgramParameter(program, gl.LINK_STATUS)) return;
    gl.useProgram(program);

    const buffer = gl.createBuffer();
    gl.bindBuffer(gl.ARRAY_BUFFER, buffer);
    gl.bufferData(gl.ARRAY_BUFFER, new Float32Array([-1,-1, 1,-1, -1,1, -1,1, 1,-1, 1,1]), gl.STATIC_DRAW);
    const position = gl.getAttribLocation(program, 'a');
    gl.enableVertexAttribArray(position);
    gl.vertexAttribPointer(position, 2, gl.FLOAT, false, 0, 0);
    const resolution = gl.getUniformLocation(program, 'r');
    const time = gl.getUniformLocation(program, 't');
    const start = performance.now();

    function compile(type, source) {
      const shader = gl.createShader(type);
      gl.shaderSource(shader, source);
      gl.compileShader(shader);
      return gl.getShaderParameter(shader, gl.COMPILE_STATUS) ? shader : null;
    }

    function resize() {
      const ratio = Math.min(window.devicePixelRatio || 1, 2);
      const width = Math.max(1, Math.floor(canvas.clientWidth * ratio));
      const height = Math.max(1, Math.floor(canvas.clientHeight * ratio));
      if (canvas.width !== width || canvas.height !== height) {
        canvas.width = width;
        canvas.height = height;
      }
      gl.viewport(0, 0, canvas.width, canvas.height);
    }

    function draw(now) {
      resize();
      gl.clearColor(0, 0, 0, 0);
      gl.clear(gl.COLOR_BUFFER_BIT);
      gl.uniform2f(resolution, canvas.width, canvas.height);
      gl.uniform1f(time, (now - start) / 1000);
      gl.drawArrays(gl.TRIANGLES, 0, 6);
      requestAnimationFrame(draw);
    }

    requestAnimationFrame(draw);
  }

  function escapeHTML(value) {
    return String(value || '').replace(/[&<>"']/g, function (char) {
      return ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[char]);
    });
  }

  renderStates();
  initAmbientGL();
})();
`
