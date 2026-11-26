package main

import (
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed web
var webFS embed.FS

// Mỗi nội dung (đôi nam nữ, đơn nam) là một dòng. Toàn bộ giải (vòng bảng + cây đấu)
// lưu thành một tài liệu JSON; version dùng để chống hai người ghi đè nhau.
const schema = `
CREATE TABLE IF NOT EXISTS tournaments (
  kind       TEXT PRIMARY KEY CHECK (kind IN ('doi','don')),
  doc        TEXT NOT NULL,
  version    INTEGER NOT NULL DEFAULT 1,
  updated_at TEXT NOT NULL
);`

// mật khẩu sửa, đặt cứng trong code; dùng chung cho cả trang.
const editPassword = "123456789"

type server struct {
	db *sql.DB
}

// knockout là thể thức loại trực tiếp: tự nhập các cặp đấu ở vòng đầu.
type knockout struct {
	Size  int            `json:"size"`  // số ô vòng đầu: 4, 8, 16
	Slots []string       `json:"slots"` // tên ở từng ô, "" = trống (đội cạnh được miễn đấu)
	Picks map[string]int `json:"picks"` // "vòng-trận" hoặc "3rd" -> ô vòng đầu
}

// doc là dữ liệu một giải. Mỗi thể thức giữ dữ liệu riêng nên đổi qua lại không mất gì.
type doc struct {
	Format  string              `json:"format"`  // "knockout" hoặc "groups"
	Ko      *knockout           `json:"ko"`      // dữ liệu loại trực tiếp
	Groups  [][]string          `json:"groups"`  // mỗi bảng 2-8 ô tên đội, "" = trống
	BestOf  int                 `json:"bestOf"`  // 1 hoặc 3 ván mỗi trận vòng bảng
	Results map[string][][]*int `json:"results"` // "bảng:ô-ô" -> [[điểm1,điểm2],...]
	Picks   map[string]int      `json:"picks"`   // lựa chọn đội thắng trên cây: "vòng-trận" hoặc "3rd" -> ô vòng đầu
	Snap    []string            `json:"snap"`    // đội đang ở mỗi ô vòng đầu khi chọn, dạng "bảng:ô"
}

type tournament struct {
	Kind string `json:"kind"`
	doc
	Version   int    `json:"version"`
	UpdatedAt string `json:"updatedAt"`
}

// số ô đội mỗi bảng (nút + / × trên trang)
const minTeams, maxTeams = 2, 8

var (
	resultKey = regexp.MustCompile(`^(\d):(\d)-(\d)$`)
	pickKey   = regexp.MustCompile(`^(\d)-(\d{1,2})$`)
	snapVal   = regexp.MustCompile(`^(\d):(\d)$`)
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	// "bracket health": dùng cho HEALTHCHECK của Docker (image distroless không có curl)
	if len(os.Args) > 1 && os.Args[1] == "health" {
		os.Exit(health())
	}

	db := openDB()

	// "bracket backup": chụp bản sao nhất quán vào <thư mục DB>/backups, giữ 10 bản gần nhất
	if len(os.Args) > 1 && os.Args[1] == "backup" {
		if err := backup(db, 10); err != nil {
			log.Fatal("backup: ", err)
		}
		return
	}

	s := &server{db: db}
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/brackets/{kind}", s.get)
	mux.HandleFunc("PUT /api/brackets/{kind}", s.put)
	mux.HandleFunc("POST /api/login", s.login)
	mux.Handle("/", http.FileServer(http.FS(sub)))

	addr := ":" + env("PORT", "8080")
	log.Println("listening on", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func openDB() *sql.DB {
	dsn := env("DB_PATH", "bracket.db") + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		log.Fatal(err)
	}
	db.SetMaxOpenConns(1) // SQLite ghi tuần tự, 1 kết nối là đủ
	if _, err := db.Exec(schema); err != nil {
		log.Fatal(err)
	}
	return db
}

// backup dùng VACUUM INTO: an toàn khi server đang chạy và đang ghi.
func backup(db *sql.DB, keep int) error {
	dir := filepath.Join(filepath.Dir(env("DB_PATH", "bracket.db")), "backups")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	out := filepath.Join(dir, "bracket-"+time.Now().UTC().Format("20060102-150405.000000")+".db")
	if _, err := db.Exec(`VACUUM INTO ?`, out); err != nil {
		return err
	}
	log.Println("đã sao lưu:", out)

	old, _ := filepath.Glob(filepath.Join(dir, "bracket-*.db"))
	sort.Strings(old) // tên theo thời gian nên sắp xếp chữ = sắp xếp thời gian
	for len(old) > keep {
		os.Remove(old[0])
		old = old[1:]
	}
	return nil
}

// health gọi API của chính server; 200 hoặc 404 (chưa có dữ liệu) đều là đang chạy tốt.
func health() int {
	c := http.Client{Timeout: 3 * time.Second}
	res, err := c.Get("http://127.0.0.1:" + env("PORT", "8080") + "/api/brackets/doi")
	if err != nil {
		log.Println("health:", err)
		return 1
	}
	res.Body.Close()
	if res.StatusCode != 200 && res.StatusCode != 404 {
		log.Println("health: status", res.StatusCode)
		return 1
	}
	return 0
}

func validKind(k string) bool { return k == "doi" || k == "don" }

func checkPassword(pw string) bool {
	return subtle.ConstantTimeCompare([]byte(pw), []byte(editPassword)) == 1
}

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	var in struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		errJSON(w, 400, "dữ liệu không đọc được")
		return
	}
	if !checkPassword(in.Password) {
		errJSON(w, 401, "sai mật khẩu")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func errJSON(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (s *server) load(kind string) (*tournament, error) {
	t := tournament{Kind: kind}
	var raw string
	err := s.db.QueryRow(
		`SELECT doc,version,updated_at FROM tournaments WHERE kind=?`, kind,
	).Scan(&raw, &t.Version, &t.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(raw), &t.doc); err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *server) get(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	if !validKind(kind) {
		errJSON(w, 400, "kind không hợp lệ")
		return
	}
	t, err := s.load(kind)
	if errors.Is(err, sql.ErrNoRows) {
		errJSON(w, 404, "chưa có dữ liệu")
		return
	}
	if err != nil {
		log.Println("load:", err)
		errJSON(w, 500, "lỗi máy chủ")
		return
	}
	writeJSON(w, 200, t)
}

func validNames(names []string) bool {
	for _, n := range names {
		if len([]rune(n)) > 30 {
			return false
		}
	}
	return true
}

func validPicks(picks map[string]int, slots int) bool {
	if len(picks) > slots {
		return false
	}
	for k, v := range picks {
		if k != "3rd" && !pickKey.MatchString(k) {
			return false
		}
		if v < 0 || v >= slots {
			return false
		}
	}
	return true
}

// validate kiểm tra dữ liệu gửi lên; trả thông báo lỗi hoặc "".
func validate(d *doc) string {
	if d.Format != "knockout" && d.Format != "groups" {
		return "thể thức phải là knockout hoặc groups"
	}
	if d.Ko == nil {
		return "thiếu dữ liệu loại trực tiếp"
	}
	if k := d.Ko; (k.Size != 4 && k.Size != 8 && k.Size != 16) || len(k.Slots) != k.Size {
		return "số ô không khớp"
	}
	if !validNames(d.Ko.Slots) {
		return "tên quá dài (tối đa 30 ký tự)"
	}
	if d.Ko.Picks == nil {
		d.Ko.Picks = map[string]int{}
	}
	if !validPicks(d.Ko.Picks, d.Ko.Size) {
		return "lựa chọn không hợp lệ"
	}

	G := len(d.Groups)
	if G != 2 && G != 4 && G != 8 {
		return "số bảng phải là 2, 4 hoặc 8"
	}
	for _, g := range d.Groups {
		if len(g) < minTeams || len(g) > maxTeams {
			return "mỗi bảng phải có từ 2 đến 8 ô"
		}
		if !validNames(g) {
			return "tên quá dài (tối đa 30 ký tự)"
		}
	}
	if d.BestOf != 1 && d.BestOf != 3 {
		return "số ván phải là 1 hoặc 3"
	}

	if d.Results == nil {
		d.Results = map[string][][]*int{}
	}
	for k, sets := range d.Results {
		m := resultKey.FindStringSubmatch(k)
		if m == nil {
			return "mã trận không hợp lệ"
		}
		g, _ := strconv.Atoi(m[1])
		i, _ := strconv.Atoi(m[2])
		j, _ := strconv.Atoi(m[3])
		if g >= G || i >= j || j >= len(d.Groups[g]) {
			return "mã trận không hợp lệ"
		}
		if len(sets) > 3 {
			return "tối đa 3 ván"
		}
		for _, set := range sets {
			if len(set) != 2 {
				return "điểm ván không hợp lệ"
			}
			for _, v := range set {
				if v != nil && (*v < 0 || *v > 99) {
					return "điểm phải từ 0 đến 99"
				}
			}
		}
	}

	P := 2 * G // số ô vòng đầu của cây
	if d.Picks == nil {
		d.Picks = map[string]int{}
	}
	if !validPicks(d.Picks, P) {
		return "lựa chọn không hợp lệ"
	}

	if d.Snap == nil {
		d.Snap = []string{}
	}
	if len(d.Snap) != 0 && len(d.Snap) != P {
		return "dữ liệu cây không khớp số bảng"
	}
	for _, v := range d.Snap {
		if v == "" {
			continue
		}
		m := snapVal.FindStringSubmatch(v)
		if m == nil {
			return "dữ liệu cây không hợp lệ"
		}
		g, _ := strconv.Atoi(m[1])
		s, _ := strconv.Atoi(m[2])
		if g >= G || s >= len(d.Groups[g]) {
			return "dữ liệu cây không hợp lệ"
		}
	}
	return ""
}

func (s *server) put(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	if !validKind(kind) {
		errJSON(w, 400, "kind không hợp lệ")
		return
	}
	if !checkPassword(r.Header.Get("X-Password")) {
		errJSON(w, 401, "sai mật khẩu")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var in struct {
		doc
		Version int `json:"version"` // 0 = tạo mới
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		errJSON(w, 400, "dữ liệu không đọc được")
		return
	}
	if msg := validate(&in.doc); msg != "" {
		errJSON(w, 400, msg)
		return
	}
	raw, _ := json.Marshal(in.doc)
	now := time.Now().UTC().Format(time.RFC3339)

	var res sql.Result
	var err error
	if in.Version == 0 {
		res, err = s.db.Exec(
			`INSERT INTO tournaments(kind,doc,version,updated_at) VALUES(?,?,1,?)
			 ON CONFLICT(kind) DO NOTHING`,
			kind, string(raw), now)
	} else {
		res, err = s.db.Exec(
			`UPDATE tournaments SET doc=?,version=version+1,updated_at=? WHERE kind=? AND version=?`,
			string(raw), now, kind, in.Version)
	}
	if err != nil {
		log.Println("save:", err)
		errJSON(w, 500, "lỗi máy chủ")
		return
	}
	affected, _ := res.RowsAffected()

	t, err := s.load(kind)
	if errors.Is(err, sql.ErrNoRows) {
		errJSON(w, 409, "chưa có dữ liệu, tải lại trang")
		return
	}
	if err != nil {
		log.Println("load:", err)
		errJSON(w, 500, "lỗi máy chủ")
		return
	}
	if affected == 0 { // có người lưu trước: trả bản mới nhất
		writeJSON(w, 409, t)
		return
	}
	writeJSON(w, 200, t)
}
