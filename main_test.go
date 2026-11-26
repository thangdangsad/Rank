package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sample = `{"format":"groups","ko":{"size":4,"slots":["A","B","C","D"],"picks":{}},
"groups":[["Thắng","Ly","Nhỉ"],["Hà","Huyền",""]],"bestOf":1,
"results":{"0:0-1":[[21,15]]},"picks":{},"snap":[],"version":%V%}`

func body(version string) *strings.Reader {
	return strings.NewReader(strings.Replace(sample, "%V%", version, 1))
}

func newTestServer(t *testing.T) (*server, *http.ServeMux) {
	t.Helper()
	t.Setenv("DB_PATH", filepath.Join(t.TempDir(), "t.db"))
	s := &server{db: openDB()}
	t.Cleanup(func() { s.db.Close() })
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/brackets/{kind}", s.get)
	mux.HandleFunc("PUT /api/brackets/{kind}", s.put)
	mux.HandleFunc("POST /api/login", s.login)
	return s, mux
}

func do(mux *http.ServeMux, method, path, password string, b *strings.Reader) *httptest.ResponseRecorder {
	var req *http.Request
	if b == nil {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, b)
	}
	if password != "" {
		req.Header.Set("X-Password", password)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func TestSaveFlow(t *testing.T) {
	_, mux := newTestServer(t)
	U := "/api/brackets/doi"
	P := editPassword

	steps := []struct {
		name, method, password, version string
		want                            int
	}{
		{"chưa có dữ liệu", "GET", "", "", 404},
		{"thiếu mật khẩu", "PUT", "", "0", 401},
		{"sai mật khẩu", "PUT", "sai", "0", 401},
		{"đúng mật khẩu, tạo mới", "PUT", P, "0", 200},
		{"tạo lại khi đã có", "PUT", P, "0", 409},
		{"cập nhật", "PUT", P, "1", 200},
		{"version cũ", "PUT", P, "1", 409},
		{"đọc lại", "GET", "", "", 200},
	}
	for _, st := range steps {
		var b *strings.Reader
		if st.method == "PUT" {
			b = body(st.version)
		}
		w := do(mux, st.method, U, st.password, b)
		if w.Code != st.want {
			t.Fatalf("%s: mã %d, muốn %d (%s)", st.name, w.Code, st.want, w.Body.String())
		}
	}
	if w := do(mux, "GET", U, "", nil); !strings.Contains(w.Body.String(), `"version":2`) ||
		!strings.Contains(w.Body.String(), "Thắng") {
		t.Fatalf("dữ liệu đọc lại sai: %s", w.Body.String())
	}
	if w := do(mux, "GET", "/api/brackets/xx", "", nil); w.Code != 400 {
		t.Fatalf("kind sai: mã %d", w.Code)
	}
}

func TestPassword(t *testing.T) {
	_, mux := newTestServer(t)
	login := func(pw string) int {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"password":"`+pw+`"}`))
		mux.ServeHTTP(w, req)
		return w.Code
	}
	if c := login(editPassword); c != 200 {
		t.Fatalf("đăng nhập đúng mật khẩu: mã %d", c)
	}
	if c := login("sai"); c != 401 {
		t.Fatalf("đăng nhập sai mật khẩu: mã %d", c)
	}
}

func TestValidate(t *testing.T) {
	ok := func() doc {
		return doc{
			Format: "groups",
			Ko:     &knockout{Size: 4, Slots: []string{"A", "B", "", ""}},
			Groups: [][]string{{"A", "B", "C"}, {"D", "E", ""}},
			BestOf: 1,
		}
	}
	d := ok()
	if msg := validate(&d); msg != "" {
		t.Fatalf("dữ liệu đúng bị từ chối: %s", msg)
	}
	n := func(v int) *int { return &v }
	bad := map[string]func(*doc){
		"thiếu format":    func(d *doc) { d.Format = "" },
		"thiếu ko":        func(d *doc) { d.Ko = nil },
		"3 bảng":          func(d *doc) { d.Groups = append(d.Groups, []string{"", "", ""}) },
		"bảng 1 ô":        func(d *doc) { d.Groups[0] = []string{"A"} },
		"bảng 9 ô":        func(d *doc) { d.Groups[0] = make([]string, 9) },
		"trận ngoài bảng": func(d *doc) { d.Results = map[string][][]*int{"1:0-3": {{n(21), n(1)}}} },
		"snap ngoài bảng": func(d *doc) { d.Snap = []string{"0:5", "", "", ""} },
		"tên dài":         func(d *doc) { d.Groups[0][0] = strings.Repeat("x", 31) },
		"5 ván":           func(d *doc) { d.BestOf = 5 },
		"mã trận sai":     func(d *doc) { d.Results = map[string][][]*int{"2:0-1": {{n(21), n(1)}}} },
		"trận tự đấu":     func(d *doc) { d.Results = map[string][][]*int{"0:1-1": {{n(21), n(1)}}} },
		"điểm 100":        func(d *doc) { d.Results = map[string][][]*int{"0:0-1": {{n(100), n(1)}}} },
		"chọn ô ngoài":    func(d *doc) { d.Picks = map[string]int{"1-0": 4} },
		"ko chọn ô sai":   func(d *doc) { d.Ko.Picks = map[string]int{"1-0": 9} },
		"ko số ô sai":     func(d *doc) { d.Ko.Size = 8 },
		"snap sai độ dài": func(d *doc) { d.Snap = []string{"0:0"} },
	}
	for name, f := range bad {
		d := ok()
		f(&d)
		if validate(&d) == "" {
			t.Errorf("%s: lẽ ra phải bị từ chối", name)
		}
	}
}

func TestBackup(t *testing.T) {
	s, _ := newTestServer(t)
	for i := 0; i < 3; i++ {
		if err := backup(s.db, 2); err != nil {
			t.Fatal(err)
		}
	}
	files, _ := filepath.Glob(filepath.Join(filepath.Dir(os.Getenv("DB_PATH")), "backups", "bracket-*.db"))
	if len(files) == 0 || len(files) > 2 {
		t.Fatalf("số bản sao lưu: %d, muốn 1-2", len(files))
	}
}

func TestValidateBigGroup(t *testing.T) {
	n := func(v int) *int { return &v }
	d := doc{
		Format: "groups",
		Ko:     &knockout{Size: 4, Slots: []string{"", "", "", ""}},
		Groups: [][]string{{"A", "B", "C", "D", "E"}, {"F", "G"}},
		BestOf: 3,
		Results: map[string][][]*int{
			"0:3-4": {{n(21), n(18)}, {n(19), n(21)}, {n(21), n(15)}},
			"1:0-1": {{n(21), n(9)}},
		},
		Snap: []string{"0:4", "1:1", "1:0", "0:3"},
	}
	if msg := validate(&d); msg != "" {
		t.Fatalf("bảng 5 đội bị từ chối: %s", msg)
	}
}
