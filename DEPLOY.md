# Deploy lên server LAN bằng Docker + GitHub Actions (self-hosted runner)

Làm giống project CV: **self-hosted runner** trên server tự kết nối ra GitHub lấy job, build image và chạy container ngay tại chỗ. Không cần mở cổng, không cần registry.

```
Nginx (cổng 80, khi có domain)
  └─► 127.0.0.1:8081 ─► container "rank" (image rank:<commit>, mở :8081 trong LAN)
                           └─► volume rank-data: /data/bracket.db (SQLite)
                                                 /data/backups/ (10 bản gần nhất)
```

CV đã dùng cổng 8080 nên Rank dùng **8081**.

| File | Vai trò |
|---|---|
| `Dockerfile` | Build 2 stage: Go build (kèm vet + test) → image distroless chỉ có binary, user nonroot |
| `deploy/docker-compose.yml` | Chạy container `rank`, publish `:8081`, gắn volume `rank-data` |
| `deploy/nginx-rank.conf` | Site Nginx cho domain riêng (khi có) |
| `.github/workflows/deploy.yml` | Ghi env tạm từ secret → build → sao lưu SQLite → `compose up` → health check → rollback nếu lỗi → xóa env tạm |

## 1. Repo: nên để private

Repo đang **public**. Workflow chỉ chạy khi push `main` hoặc bấm tay (không chạy theo PR), nên người ngoài không chạy được code trên server qua PR. GitHub vẫn khuyên **không dùng self-hosted runner với repo public**: nếu sau này có ai thêm workflow chạy theo `pull_request`, server sẽ bị lộ. Đổi private: repo → Settings → General → Danger Zone → Change visibility.

## 2. Secret `ENV_FILE`

GitHub repo → **Settings → Environments → ENV_FILE** → **Environment secrets** → secret `ENV_FILE`, nội dung theo `.env.example` (workflow khai báo `environment: ENV_FILE` để đọc được secret này):

```
EDIT_TOKEN=ma-bi-mat-cua-ban
```

Hoặc từ máy dev: `gh secret set ENV_FILE --env ENV_FILE < .env` (file giữ ở máy bạn, không commit).

Nên đặt **Deployment branches and tags** của environment này thành **Selected branches → `main`**: chỉ code trên `main` mới đọc được secret.

Đổi mã = sửa secret rồi chạy lại workflow (**Actions → deploy → Run workflow**). `docker restart rank` không đủ: env chỉ được nạp khi tạo container.

## 3. Cài runner cho repo Rank (một lần)

Runner của CV gắn với repo CV, không lấy job của repo Rank. Cài thêm một runner nữa trên cùng server, cùng user `deploy` (đã thuộc group docker từ khi cài CV):

1. GitHub repo Rank → **Settings → Actions → Runners → New self-hosted runner** → **Linux**, đúng kiến trúc.
2. Chạy các lệnh GitHub hiện ra **bằng user deploy**, trong **thư mục riêng**:

```bash
sudo -iu deploy
mkdir actions-runner-rank && cd actions-runner-rank
curl -o runner.tar.gz -L <link trên trang GitHub>
tar xzf runner.tar.gz
./config.sh --url https://github.com/thangdangsad/Rank --token <token trên trang GitHub> --name rank-lan
exit
```

3. Cài thành service (user admin có sudo):

```bash
sudo bash -c 'cd /home/deploy/actions-runner-rank && ./svc.sh install deploy && ./svc.sh start && ./svc.sh status'
```

Kiểm tra: trang **Runners** của repo Rank hiện **Idle**.

## 4. Deploy

Push lên `main` (hoặc **Actions → deploy → Run workflow**). Workflow chạy trên server:

1. `docker build` (trong đó chạy `go vet` + `go test`; test fail → không có image)
2. Sao lưu SQLite của container đang chạy (`/bracket backup`, giữ 10 bản trong volume)
3. `docker compose up -d` với tag `rank:<commit>`
4. Health check `GET /api/brackets/doi` (200 hoặc 404 là OK); lỗi thì chạy lại tag trước đó
5. Xóa image cũ, giữ 5 bản gần nhất

Mở `http://192.168.1.117:8081/`.

## Khi có domain public

1. Trỏ DNS `A` về IP public, mở cổng 80/443 vào server.
2. Sửa `server_name` trong `deploy/nginx-rank.conf`, chép vào `/etc/nginx/sites-available/rank`:

```bash
sudo ln -sf /etc/nginx/sites-available/rank /etc/nginx/sites-enabled/rank
sudo nginx -t && sudo systemctl reload nginx
sudo certbot --nginx -d rank.domain.com
```

Cổng 80 theo IP đã thuộc site CV, nên Rank cần domain riêng để đi qua Nginx.

## Vận hành

```bash
docker logs -f rank                                  # log
docker ps --filter name=rank                         # trạng thái + (healthy)
docker inspect rank --format '{{.Config.Image}}'     # commit đang chạy
docker exec rank /bracket backup                     # sao lưu tay
```

Chép bản sao lưu ra ngoài server (container không có shell, dùng `docker cp`):

```bash
docker exec rank /bracket backup
docker cp rank:/data/backups ./rank-backups
```

Khôi phục một bản sao lưu:

```bash
docker stop rank
docker run --rm -v rank-data:/data -v "$PWD/rank-backups:/b:ro" alpine \
  sh -c 'cp /b/<file>.db /data/bracket.db && rm -f /data/bracket.db-wal /data/bracket.db-shm && chown 65532:65532 /data/bracket.db'
docker start rank
```

Rollback về commit cũ: **Actions → deploy → Run workflow** trên commit đó (hoặc revert rồi push). Workflow tự rollback khi health check lỗi.
