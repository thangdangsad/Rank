# Cây xếp hạng giải cầu lông: lưu dữ liệu bằng Go + SQLite

## 1. Kết luận ngắn

- **Nên dùng database.** Hiện trang chỉ lưu bằng `localStorage` của trình duyệt, nên đổi máy, xóa dữ liệu trình duyệt hoặc mở bằng điện thoại khác là mất hết, và người khác không xem được kết quả.
- **Chọn SQLite, không cần Postgres/MySQL.** Giải có 12 người, 2 cây, vài chục lượt ghi. SQLite là một file duy nhất, không cần cài server riêng, backup bằng cách chép file.
- **Backend Go + frontend HTML là hợp lý.** Go biên dịch ra 1 file chạy, nhúng luôn cả trang HTML vào bên trong (`go:embed`), nên triển khai chỉ cần chép 1 file và 1 thư mục dữ liệu.
- **Lưu ý quan trọng:** trang đang đăng trên claude.ai không gọi được backend riêng (trình duyệt chặn gọi sang domain khác). Muốn lưu vào database, trang HTML phải được chính server Go phục vụ.

| Phương án | Ưu | Nhược | Phù hợp |
|---|---|---|---|
| `localStorage` (hiện tại) | Không cần server | Mất khi đổi máy, không chia sẻ | Chỉ để thử |
| File JSON trên server | Rất đơn giản | Dễ hỏng khi 2 người ghi cùng lúc | Chỉ 1 người nhập |
| **SQLite (khuyên dùng)** | 1 file, ghi an toàn, có giao dịch | Chỉ chạy trên 1 máy | **Giải này** |
| Postgres | Nhiều server, nhiều giải | Phải cài và quản trị | Khi làm nhiều giải, nhiều người |

## 2. Kiến trúc

```
Điện thoại / máy tính
   │  HTTPS
   ▼
Caddy (tự cấp HTTPS)  ──►  Go server (1 file chạy)
                              ├─ phục vụ web/index.html (nhúng sẵn)
                              ├─ GET  /api/brackets/{kind}   đọc cây
                              └─ PUT  /api/brackets/{kind}   lưu cây
                                        │
                                        ▼
                                  bracket.db (SQLite)
```

`kind` là `doi` (đôi nam nữ) hoặc `don` (đơn nam), giống 2 tab hiện tại.

## 3. Cấu trúc thư mục

```
bracket/
├─ main.go
├─ go.mod
├─ Dockerfile
└─ web/
   └─ index.html      # trang cây xếp hạng, sửa phần lưu trữ (mục 6)
```

Khởi tạo:

```bash
mkdir bracket && cd bracket
go mod init bracket
go get modernc.org/sqlite      # SQLite thuần Go, không cần cgo/gcc
mkdir web                       # chép index.html vào đây
```

## 4. Cơ sở dữ liệu

Mỗi cây là một dòng. Ô vòng đầu (`slots`) và các lựa chọn đội thắng (`picks`) lưu dạng JSON, đúng như trang đang dùng, nên không phải đổi logic vẽ cây.

```sql
CREATE TABLE IF NOT EXISTS brackets (
  kind        TEXT PRIMARY KEY CHECK (kind IN ('doi','don')),
  size        INTEGER NOT NULL CHECK (size IN (4,8,16)),
  slots       TEXT NOT NULL,              -- ["Thắng-Trang","", ...]
  picks       TEXT NOT NULL,              -- {"1-1":3,"2-0":1,"3rd":2}
  version     INTEGER NOT NULL DEFAULT 1, -- tăng mỗi lần lưu, chống ghi đè nhau
  updated_at  TEXT NOT NULL
);
```

`version` dùng để tránh việc 2 người cùng sửa rồi người sau ghi đè người trước. Nếu `version` gửi lên không khớp, server trả `409` kèm bản mới nhất.

Nếu sau này muốn lưu cả vòng bảng (lịch, kết quả từng trận), thêm các bảng `teams`, `matches` (đội 1, đội 2, số ván thắng, tổng điểm). Phần cây xếp hạng không cần đổi.

## 5. Backend Go (`main.go`)

```go
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
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed web
var webFS embed.FS

const schema = `
CREATE TABLE IF NOT EXISTS brackets (
  kind       TEXT PRIMARY KEY CHECK (kind IN ('doi','don')),
  size       INTEGER NOT NULL CHECK (size IN (4,8,16)),
  slots      TEXT NOT NULL,
  picks      TEXT NOT NULL,
  version    INTEGER NOT NULL DEFAULT 1,
  updated_at TEXT NOT NULL
);`

type server struct {
	db    *sql.DB
	token string // mã chỉnh sửa; để trống = ai cũng sửa được
}

type bracket struct {
	Kind      string         `json:"kind"`
	Size      int            `json:"size"`
	Slots     []string       `json:"slots"`
	Picks     map[string]int `json:"picks"`
	Version   int            `json:"version"`
	UpdatedAt string         `json:"updatedAt"`
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	dsn := env("DB_PATH", "bracket.db") + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		log.Fatal(err)
	}
	db.SetMaxOpenConns(1) // SQLite ghi tuần tự, 1 kết nối là đủ
	if _, err := db.Exec(schema); err != nil {
		log.Fatal(err)
	}

	s := &server{db: db, token: os.Getenv("EDIT_TOKEN")}
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/brackets/{kind}", s.get)
	mux.HandleFunc("PUT /api/brackets/{kind}", s.put)
	mux.Handle("/", http.FileServer(http.FS(sub)))

	addr := ":" + env("PORT", "8080")
	log.Println("listening on", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func validKind(k string) bool { return k == "doi" || k == "don" }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (s *server) load(kind string) (*bracket, error) {
	var b bracket
	var slots, picks string
	err := s.db.QueryRow(
		`SELECT kind,size,slots,picks,version,updated_at FROM brackets WHERE kind=?`, kind,
	).Scan(&b.Kind, &b.Size, &slots, &picks, &b.Version, &b.UpdatedAt)
	if err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(slots), &b.Slots)
	json.Unmarshal([]byte(picks), &b.Picks)
	if b.Picks == nil {
		b.Picks = map[string]int{}
	}
	return &b, nil
}

func (s *server) get(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	if !validKind(kind) {
		writeJSON(w, 400, map[string]string{"error": "kind không hợp lệ"})
		return
	}
	b, err := s.load(kind)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, 404, map[string]string{"error": "chưa có dữ liệu"})
		return
	}
	if err != nil {
		log.Println("load:", err)
		writeJSON(w, 500, map[string]string{"error": "lỗi máy chủ"})
		return
	}
	writeJSON(w, 200, b)
}

func (s *server) authorized(r *http.Request) bool {
	if s.token == "" {
		return true
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) == 1
}

func (s *server) put(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	if !validKind(kind) {
		writeJSON(w, 400, map[string]string{"error": "kind không hợp lệ"})
		return
	}
	if !s.authorized(r) {
		writeJSON(w, 401, map[string]string{"error": "cần mã chỉnh sửa"})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var in struct {
		Size    int            `json:"size"`
		Slots   []string       `json:"slots"`
		Picks   map[string]int `json:"picks"`
		Version int            `json:"version"` // 0 = tạo mới
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "dữ liệu không đọc được"})
		return
	}
	if (in.Size != 4 && in.Size != 8 && in.Size != 16) || len(in.Slots) != in.Size {
		writeJSON(w, 400, map[string]string{"error": "số ô không khớp"})
		return
	}
	for _, v := range in.Slots {
		if len([]rune(v)) > 30 {
			writeJSON(w, 400, map[string]string{"error": "tên quá dài (tối đa 30 ký tự)"})
			return
		}
	}
	if in.Picks == nil {
		in.Picks = map[string]int{}
	}
	slotsJSON, _ := json.Marshal(in.Slots)
	picksJSON, _ := json.Marshal(in.Picks)
	now := time.Now().UTC().Format(time.RFC3339)

	var affected int64
	if in.Version == 0 {
		res, err := s.db.Exec(
			`INSERT INTO brackets(kind,size,slots,picks,version,updated_at)
			 VALUES(?,?,?,?,1,?) ON CONFLICT(kind) DO NOTHING`,
			kind, in.Size, string(slotsJSON), string(picksJSON), now)
		if err != nil {
			log.Println("insert:", err)
			writeJSON(w, 500, map[string]string{"error": "lỗi máy chủ"})
			return
		}
		affected, _ = res.RowsAffected()
	} else {
		res, err := s.db.Exec(
			`UPDATE brackets SET size=?,slots=?,picks=?,version=version+1,updated_at=?
			 WHERE kind=? AND version=?`,
			in.Size, string(slotsJSON), string(picksJSON), now, kind, in.Version)
		if err != nil {
			log.Println("update:", err)
			writeJSON(w, 500, map[string]string{"error": "lỗi máy chủ"})
			return
		}
		affected, _ = res.RowsAffected()
	}

	b, err := s.load(kind)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "lỗi máy chủ"})
		return
	}
	if affected == 0 { // có người lưu trước: trả bản mới nhất
		writeJSON(w, 409, b)
		return
	}
	writeJSON(w, 200, b)
}
```

Chạy thử:

```bash
go run .
# mở http://localhost:8080
```

## 6. Sửa trang HTML để lưu qua API

Trong `web/index.html`, thay phần `localStorage` bằng gọi API. Giữ nguyên toàn bộ phần vẽ cây.

```js
const API = "/api/brackets/";
let token = "";
try { token = localStorage.getItem("editToken") || ""; } catch (e) {}
const timers = {};

function setStatus(t) { document.getElementById("status").textContent = t; }

// Gọi 1 lần khi mở trang, trước render() đầu tiên
async function loadAll() {
  for (const kind of ["doi", "don"]) {
    try {
      const r = await fetch(API + kind);
      if (r.ok) {
        const b = await r.json();
        state.data[kind] = { size: b.size, slots: b.slots, picks: b.picks, version: b.version };
      }
    } catch (e) { setStatus("Không tải được dữ liệu"); }
  }
}

// Thay cho hàm save() cũ: gọi sau mỗi thay đổi
function save() {
  const kind = state.tab;
  clearTimeout(timers[kind]);
  setStatus("Đang lưu...");
  timers[kind] = setTimeout(() => saveNow(kind), 400); // gộp nhiều lần gõ thành 1 lần lưu
}

async function saveNow(kind) {
  const d = state.data[kind];
  try {
    const r = await fetch(API + kind, {
      method: "PUT",
      headers: { "Content-Type": "application/json", "Authorization": "Bearer " + token },
      body: JSON.stringify({ size: d.size, slots: d.slots, picks: d.picks, version: d.version || 0 }),
    });
    const b = await r.json();
    if (r.ok) { d.version = b.version; setStatus("Đã lưu"); }
    else if (r.status === 409) {
      state.data[kind] = { size: b.size, slots: b.slots, picks: b.picks, version: b.version };
      render();
      setStatus("Có người vừa sửa, đã tải bản mới nhất");
    }
    else if (r.status === 401) setStatus("Cần nhập mã chỉnh sửa để lưu");
    else setStatus("Lưu lỗi, thử lại");
  } catch (e) { setStatus("Mất kết nối, chưa lưu được"); }
}
```

Thêm vào HTML một dòng trạng thái (ví dụ cạnh tiêu đề cây): `<span id="status" class="hint"></span>`.

Cuối file, đổi `load(); render();` thành: `loadAll().then(render);`.

Ai chỉ xem thì không cần mã. Người nhập kết quả nhập mã một lần (lưu `editToken` vào `localStorage`), ví dụ bằng một ô nhập nhỏ.

## 7. Triển khai

Build:

```bash
CGO_ENABLED=0 go build -o bracket .
PORT=8080 DB_PATH=./data/bracket.db EDIT_TOKEN=doi-ma-nay ./bracket
```

Docker (`Dockerfile`):

```dockerfile
FROM golang:1.23 AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /bracket .

FROM gcr.io/distroless/static
COPY --from=build /bracket /bracket
ENV DB_PATH=/data/bracket.db
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/bracket"]
```

```bash
docker build -t bracket .
docker run -d --name bracket -p 8080:8080 -v bracket-data:/data \
  -e EDIT_TOKEN=doi-ma-nay bracket
```

Chỗ chạy: một VPS nhỏ, hoặc Fly.io / Railway (nhớ gắn volume cho thư mục `/data`, nếu không dữ liệu mất mỗi lần deploy). HTTPS dùng Caddy hoặc do nền tảng cấp sẵn.

## 8. Sao lưu

- Chép file `bracket.db` (kèm `bracket.db-wal` nếu có) sang nơi khác sau mỗi buổi thi đấu.
- Hoặc dùng `sqlite3 bracket.db ".backup backup.db"` để có bản sao nhất quán khi server đang chạy.
- Thêm nút "Xuất JSON" trong trang nếu muốn tự giữ một bản.

## 9. Kiểm tra bằng curl

```bash
# chưa có dữ liệu -> 404
curl -i localhost:8080/api/brackets/doi

# tạo mới (version 0)
curl -i -X PUT localhost:8080/api/brackets/doi \
  -H 'Authorization: Bearer doi-ma-nay' -H 'Content-Type: application/json' \
  -d '{"size":4,"slots":["A","B","C","D"],"picks":{},"version":0}'

# đọc lại -> version 1
curl localhost:8080/api/brackets/doi

# lưu với version cũ -> 409 (chống ghi đè)
curl -i -X PUT localhost:8080/api/brackets/doi \
  -H 'Authorization: Bearer doi-ma-nay' -H 'Content-Type: application/json' \
  -d '{"size":4,"slots":["A","B","C","D"],"picks":{"1-0":0},"version":5}'
```

## 10. Việc cần làm theo thứ tự

1. Tạo thư mục, `go mod init`, chép `main.go` và `web/index.html`.
2. Chạy thử `go run .`, kiểm tra bằng curl (mục 9).
3. Sửa `index.html` theo mục 6, mở trang bằng 2 trình duyệt để thử 409.
4. Đặt `EDIT_TOKEN`, đóng gói Docker, đưa lên server có volume.
5. Gắn tên miền và HTTPS, gửi link cho mọi người xem.
6. Lên lịch sao lưu file `bracket.db`.
