# Kiro-Go — Hướng dẫn triển khai (Docker)

Tài liệu này mô tả cách triển khai Kiro-Go đúng cách bằng Docker Compose, giải
thích cơ chế loopback port của luồng đăng nhập SSO, và liệt kê các lỗi thường gặp
cùng cách khắc phục.

---

## 1. Tổng quan

Kiro-Go là reverse proxy dịch request Kiro API sang định dạng OpenAI/Anthropic,
kèm admin panel quản lý tài khoản. Service expose:

| Endpoint | Mô tả |
|----------|-------|
| `/admin` | Admin panel (web UI) |
| `/v1/messages` | Claude API compatible |
| `/v1/chat/completions` | OpenAI API compatible |

Cổng chính: **8080**. Mật khẩu admin mặc định: **`changeme`** (đổi qua
`ADMIN_PASSWORD`, xem mục 5).

---

## 2. Triển khai chuẩn (Docker Compose)

Đây là cách **được khuyến nghị** — cấu hình nằm trong `docker-compose.yml`,
version-controlled, tái lập được.

```bash
# Từ thư mục gốc repo
docker compose up -d --build
```

- `--build` ép build image từ source hiện tại. **Bắt buộc dùng khi code thay đổi**
  — nếu bỏ qua, compose sẽ tái sử dụng image cache cũ và chạy nhầm version cũ
  (xem Lỗi #3 ở mục 6).
- `-d` chạy nền (detached).

Kiểm tra:

```bash
docker compose ps                       # container Up?
curl -s -o /dev/null -w '%{http_code}' http://localhost:8080/admin   # mong đợi 200
```

Mở admin panel: <http://localhost:8080/admin> → nhập mật khẩu (`changeme`).

Dừng / khởi động lại:

```bash
docker compose down        # dừng + xoá container (data giữ nguyên nhờ volume mount)
docker compose up -d       # chạy lại (không build lại)
docker compose up -d --build   # chạy lại CÓ build lại từ source
```

---

## 3. Cấu hình `docker-compose.yml`

```yaml
services:
  kiro-go:
    build: .
    ports:
      - "${KIRO_PORT:-8080}:8080"    # admin panel + API (host port đổi được)
      - "127.0.0.1:3128:3128"        # callback Enterprise SSO, chỉ loopback host
    volumes:
      - ./data:/app/data             # persist config + accounts
    environment:
      - CONFIG_PATH=/app/data/config.json
      - KIRO_SSO_CALLBACK_BIND=0.0.0.0   # cần thiết trong Docker — xem mục 4
    restart: unless-stopped
```

> Đây là bản rút gọn cho dễ đọc. File `docker-compose.yml` thật trong repo là
> nguồn chính xác duy nhất và có thêm mount `data/imports`, mount AWS SSO cache,
> `PORT`/`HOST`, `KIRO_IMPORT_WATCH` và healthcheck. Đọc file đó trước khi sửa.

**Các điểm quan trọng:**

- **`KIRO_SSO_CALLBACK_BIND=0.0.0.0`** — cần khi chạy trong container. Mặc định
  listener callback chỉ bind loopback (`127.0.0.1` + `[::1]`), mà cổng publish
  không tới được loopback trong container, nên login Enterprise SSO sẽ treo.
  Đây là **biến duy nhất** tree này đọc cho việc đó (`auth/kiro_sso.go:299`).
- **`LOOPBACK_HOST` KHÔNG còn tác dụng.** Bản upstream v1.2.8 dùng biến này cho
  cùng mục đích, nhưng reader Go của nó không sống sót qua merge:
  `grep -rn LOOPBACK_HOST --include='*.go'` không khớp gì. Đặt nó trông như
  cấu hình nhưng không đổi hành vi nào.
- **`./data:/app/data`** — mount này giữ `config.json` (gồm accounts, stats, mật
  khẩu). Nhờ nó, xoá/tạo lại container **không mất dữ liệu**.
- **Chỉ publish 1 port SSO (3128).** Listener bind cố định `kiroRedirectPort =
  "3128"` (`auth/kiro_sso.go:62`), không quét danh sách port. Các port
  `4649/6588/8008/9091` trong tài liệu cũ không còn cần publish.
- **Publish host-side `127.0.0.1:3128`** giữ callback khỏi interface ngoài. Hệ quả:
  browser đăng nhập **phải chạy trên chính Docker host**.

---

## 4. Cơ chế loopback port (luồng SSO)

Luồng "Add Account → Kiro Hosted SSO" (đăng nhập Microsoft Entra qua Kiro portal)
dùng OAuth loopback redirect. Khi bấm **Start Login**, backend:

1. Bind HTTP callback server trên **một port cố định**: `3128`
   (`auth/kiro_sso.go:62`, `kiroRedirectPort = "3128"`). Địa chỉ bind do
   `kiroCallbackBindAddrs()` quyết định (`auth/kiro_sso.go:298-303`):

   - mặc định: `127.0.0.1:3128` **và** `[::1]:3128` (browser giải "localhost"
     có thể ra IPv4 hoặc IPv6, nên bind cả hai — địa chỉ đầu là bắt buộc, địa
     chỉ sau best-effort);
   - nếu `KIRO_SSO_CALLBACK_BIND` được set: chỉ bind `<giá trị>:3128`.

2. Sinh Login URL trỏ về `redirect_uri=http://localhost:3128` và trả cho UI.
3. Browser mở URL → đăng nhập Microsoft → Microsoft redirect về callback server →
   app đổi code lấy token → tạo account.

**Chỉ cần publish đúng 1 port SSO.** Không có cơ chế quét nhiều port trong tree
này: port là hằng số, nên `3128` phải trống trên host. Nếu `3128` đã bị chiếm,
login sẽ fail với lỗi bind (`cannot bind ... for the SSO callback`) chứ **không**
tự nhảy sang port khác.

> **Lưu ý cho ai đọc tài liệu cũ:** bản trước mô tả danh sách 10 port
> (`3128, 4649, 6588, 8008, 9091, 49153…53153`) và hàm `kiroLoopbackPorts` /
> `bindKiroLoopback`. Cơ chế đó thuộc bản upstream v1.2.8 và **không có** trong
> tree này — `grep -rn kiroLoopbackPorts --include='*.go'` không khớp gì. Đừng
> publish 4649/6588/8008/9091 nữa; chúng không được dùng.

---

## 5. Biến môi trường

| Biến | Mặc định | Mô tả |
|------|----------|-------|
| `CONFIG_PATH` | `data/config.json` | Đường dẫn file config |
| `KIRO_SSO_CALLBACK_BIND` | `127.0.0.1` + `[::1]` | Host bind listener callback Enterprise SSO. **Đặt `0.0.0.0` trong Docker.** |
| `ADMIN_PASSWORD` | (dùng giá trị trong config, mặc định `changeme`) | Ghi đè mật khẩu admin lúc khởi động |
| `LOG_LEVEL` | `info` | Mức log (`debug`/`info`/`warn`/`error`) |

> Bảng này chỉ liệt kê các biến hay dùng khi deploy. Danh sách **đầy đủ** các biến
> mà code thật sự đọc nằm ở [README.md](README.md#environment-variables) — đã đối
> chiếu với `os.Getenv` trong source. `LOOPBACK_HOST` **không** nằm trong đó:
> tree này không đọc nó (xem mục 3).

Ví dụ đổi mật khẩu admin:

```yaml
    environment:
      - KIRO_SSO_CALLBACK_BIND=0.0.0.0
      - ADMIN_PASSWORD=my-strong-password
```

---

## 6. Lỗi thường gặp & cách khắc phục

### Lỗi #1 — Start Login trả HTTP 500, không bind được callback

**Triệu chứng:** Bấm Start Login → console báo `500` tại
`/admin/api/auth/kiro-sso/start`. Log server chứa:

```
cannot bind 127.0.0.1:3128 for the SSO callback (is the port already in use?)
```

(chuỗi lỗi thật ở `auth/kiro_sso.go:314`)

**Nguyên nhân:** port `3128` đã bị tiến trình khác chiếm, **hoặc**
`KIRO_SSO_CALLBACK_BIND` được set thành một địa chỉ không bind được (ví dụ thiếu
octet: `0.0.0` thay vì `0.0.0.0`, hoặc một IP không tồn tại trên máy). Port là
**hằng số** `3128` — không có fallback sang port khác, nên bind fail là fail hẳn.

**Khắc phục:**

```bash
# 1. Ai đang giữ 3128 trên host?
ss -ltnp | grep ':3128' || echo 'port trống'

# 2. Giá trị bind thật trong container (đúng tên biến)
docker compose exec kiro-go printenv KIRO_SSO_CALLBACK_BIND   # mong đợi: 0.0.0.0

# 3. Nếu sai: sửa docker-compose.yml rồi recreate
docker compose up -d --force-recreate
```

> Env được baked vào container lúc tạo — sửa file compose thôi chưa đủ, phải
> recreate container.

> **Đừng kiểm tra `LOOPBACK_HOST`.** Tài liệu cũ hướng dẫn `printenv LOOPBACK_HOST`,
> nhưng tree này không đọc biến đó, nên nó luôn "trông sai" và làm lệch hướng
> debug. Biến đúng là `KIRO_SSO_CALLBACK_BIND`.

### Lỗi #2 — Compose fail: "ports are not available: ... 49153: address already in use"

**Triệu chứng:** `docker compose up` fail khi bind port `49153` (hoặc 5015x),
dù `lsof` báo port đó trống vài giây trước.

**Nguyên nhân:** Trên macOS, dải `49153–53153` nằm **trong vùng ephemeral port**
(`sysctl net.inet.ip.portrange.first` = 49152). OS liên tục cấp các port này làm
source port cho kết nối outbound, nên việc bind cố định bị race và fail bất chợt.

**Khắc phục:** Tree này **không dùng** các port đó — callback SSO bind cố định
`3128` (xem mục 4). File `docker-compose.yml` trong repo chỉ publish
`${KIRO_PORT:-8080}:8080` và `127.0.0.1:3128:3128`. Nếu file của bạn vẫn còn các
dòng `49153`–`53153` (hoặc `4649/6588/8008/9091`), xoá hết đi.

### Lỗi #3 — App chạy version cũ sau khi sửa code

**Triệu chứng:** Footer admin panel hiển thị version cũ dù `version.json` trên đĩa
đã mới hơn.

**Nguyên nhân:** `docker compose up` **không tự rebuild** khi image đã tồn tại
trong cache — nó tái dùng image cũ.

**Khắc phục:**

```bash
docker compose up -d --build   # ép rebuild từ source
```

### Lỗi #4 — Trộn lẫn `docker run` thủ công và `docker compose`

**Triệu chứng:** Có hai container Kiro song song, tên khác nhau
(`kiro-go` vs `kiro-go-kiro-go-1`), đụng port nhau.

**Nguyên nhân:** Container tạo bằng `docker run` thủ công độc lập với container do
compose quản lý. Compose không "thấy" container thủ công.

**Khắc phục — thống nhất một cách duy nhất (compose):**

```bash
# Xoá mọi container Kiro tạo thủ công
docker ps -a --filter ancestor=kiro-go --format '{{.Names}}' | xargs -r docker rm -f

# Từ nay chỉ dùng compose
docker compose up -d --build
```

---

## 7. Build thủ công (không khuyến khích)

Chỉ dùng khi cần chạy ngoài compose. Lưu ý phải tự đặt đúng
`KIRO_SSO_CALLBACK_BIND` và publish port:

```bash
docker build -t kiro-go .
docker run -d --name kiro-go \
  -p 8080:8080 -p 127.0.0.1:3128:3128 \
  -e CONFIG_PATH=/app/data/config.json \
  -e KIRO_SSO_CALLBACK_BIND=0.0.0.0 \
  -v "$(pwd)/data:/app/data" \
  kiro-go
```

> Chỉ cần 2 port: `8080` cho admin/API và `3128` cho callback SSO. Các port
> `4649/6588/8008/9091` trong tài liệu cũ không được dùng (xem mục 4).

> Đây chính là nguồn gốc các lỗi #1, #2, #4 ở trên (sai tên/giá trị env, dư port,
> trộn lẫn với compose). Nếu không có lý do đặc biệt, hãy dùng `docker compose` ở
> mục 2.

---

## 8. Chạy không cần Docker (dev local)

```bash
go build -o kiro-go .
./kiro-go      # callback SSO bind 127.0.0.1:3128 + [::1]:3128; HTTP theo config
```

Mở <http://localhost:8080/admin>. Khi chạy local (không container), browser và
callback listener cùng máy nên **không cần** đặt `KIRO_SSO_CALLBACK_BIND`.

> Host/port của HTTP server lấy theo thứ tự: flag `-host`/`-port` > env
> `HOST`/`PORT` > `config.json`. Ví dụ `./kiro-go -port 9090` hoặc
> `PORT=9090 ./kiro-go`.

> **Chạy từ thư mục gốc repo.** `/readyz` kiểm tra `data` ghi được và
> `web/index.html` tồn tại theo đường dẫn **tương đối** với working directory
> (`proxy/handler.go:4583-4586`), nên chạy binary từ chỗ khác sẽ báo `degraded`
> dù server vẫn hoạt động.

> Để phát triển mà **không** chạm credentials thật, dùng `scripts/dev.sh` — nó
> build vào temp dir và trỏ `CONFIG_PATH` vào file tạm. Xem
> [docs/tutorials/01-local-development.md](docs/tutorials/01-local-development.md).
