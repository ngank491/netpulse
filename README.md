# NetPulse – giám sát MikroTik gọn nhẹ

Một file chạy duy nhất (~13 MB, ~16 MB RAM), không cần Node.js, Docker hay database riêng.
Chạy trên Windows, Linux, macOS, Raspberry Pi, Docker và Home Assistant OS.

## Tính năng

- **Tổng quan nhiều router**: CPU, RAM, uptime, phiên bản, nhiệt độ, trạng thái WAN và số thiết bị lỗi trên một màn hình.
- **Hai cách kết nối router**
  - **REST API (khuyên dùng)**: NetPulse tự đọc router bằng user chỉ-đọc. Không cài script, không đổi device-mode, không khởi động lại router. Cần RouterOS 7.1+.
  - **Push**: router chạy script gửi dữ liệu lên NetPulse. Dành cho router ở xa, sau NAT, không có VPN. Cần RouterOS 7.17+.
- **WAN đa đường**: theo dõi link và Internet (ping qua từng interface) của từng đường truyền. Tự gợi ý WAN từ PPPoE client, DHCP client, interface list WAN và route mặc định.
- **Theo dõi thiết bị**: router ping camera, switch, AP, NAS… mỗi 30 giây. Hiển thị độ trễ và tỷ lệ sẵn sàng 24 giờ. Thêm nhanh từ danh sách DHCP hoặc MNDP/LLDP neighbor.
- **Sơ đồ mạng kéo thả**: nút đổi màu theo trạng thái, có nút ảo cho switch không quản lý được, nút "Tự sắp xếp".
- **Biểu đồ lịch sử** lưu lượng theo interface, CPU và RAM: 1 giờ đến 30 ngày.
- **Client**: gộp DHCP lease và ARP; phiên PPPoE; địa chỉ IP, route mặc định, neighbor.
- **Cảnh báo Telegram và Zalo**
  - Không báo động giả: chỉ báo mất kết nối sau 2 lần kiểm tra liên tiếp thất bại; lỗi chạy lệnh ping không được tính.
  - Không mất tin: cảnh báo được ghi vào database trước khi gửi, rồi gửi lại khi có mạng (giữ tối đa 24 giờ). Nhiều cảnh báo xảy ra cùng lúc được gộp thành một tin.
  - Kênh quản trị nhận mọi cảnh báo; mỗi router có thể có nhóm Telegram riêng.
  - Nhắc lại khi lỗi chưa được khắc phục. Bản tin tổng hợp hằng ngày.
  - Bot trả lời `/status` (xem tình trạng) và `/start` hoặc `/id` (cho biết Chat ID).
- **Phân quyền**: quản trị viên và người xem. Người xem chỉ thấy các router được giao và bị chặn ở cả giao diện lẫn API.
- **Sao lưu một chạm** khi đang chạy (bản sao database nhất quán).

📘 **Hướng dẫn chi tiết thêm router, Telegram và Zalo: [docs/HUONG-DAN.md](docs/HUONG-DAN.md)**

## So với repo NetWatch gốc

| | NetWatch | NetPulse |
|---|---|---|
| Runtime | Node.js + React + better-sqlite3 (native) | 1 file Go tĩnh, SQLite thuần Go |
| Kích thước gói Windows | Node.js đi kèm (~80 MB) | 13 MB |
| Kết nối router | Bắt buộc script + device-mode + khởi động lại router | REST API chỉ-đọc, hoặc script nếu cần |
| HTTPS | Phải tự dựng reverse proxy | `-tls auto` (chứng chỉ tự ký) |
| Đổi mật khẩu admin | Phải xoá `auth.json` (mất luôn khoá giải mã Bot Token) | Trong giao diện, hoặc lệnh `reset-password` |
| Đăng xuất / thu hồi phiên | Không thu hồi được | Phiên lưu trong DB, đăng xuất là hết hiệu lực |
| Giới hạn số lần đăng nhập sai | Theo IP (sau proxy có thể khoá nhầm admin) | Theo IP + tên đăng nhập, thời gian khoá tăng dần |
| Lọc dữ liệu cho người xem | Xoá bớt trường sau khi trả (dễ lộ trường mới) | Kiểm tra quyền theo router ngay đầu mỗi API |
| Mã nguồn | Viết nén, dòng dài hơn 5.000 ký tự | Dễ đọc, có test (kể cả race detector) |
| Giấy phép | Không có | MIT |

## Cài đặt

### Windows

1. Tải `netpulse-…-windows-amd64.zip` ở trang Releases rồi giải nén.
2. Nhấp đúp `netpulse.exe`. Cửa sổ sẽ hiện **mã khởi tạo**.
3. Mở `http://IP-máy-này:8080`, nhập mã khởi tạo rồi tạo tài khoản quản trị.

Dữ liệu nằm trong thư mục `data` cạnh file exe. Khi nâng cấp, chỉ cần thay file exe.

### Linux / Raspberry Pi

```bash
tar xzf netpulse-*-linux-arm64.tar.gz && cd netpulse-*/
./netpulse -data /var/lib/netpulse
```

Ví dụ chạy như dịch vụ systemd (`/etc/systemd/system/netpulse.service`):

```ini
[Unit]
Description=NetPulse
After=network-online.target
[Service]
ExecStart=/usr/local/bin/netpulse -data /var/lib/netpulse
DynamicUser=yes
StateDirectory=netpulse
Restart=always
[Install]
WantedBy=multi-user.target
```

### Docker

```bash
docker run -d --name netpulse --restart unless-stopped -p 8080:8080 -v netpulse-data:/data ghcr.io/OWNER/netpulse:latest
```

```bash
docker logs netpulse
```

Lệnh thứ hai hiện mã khởi tạo. Có thể dùng `compose.yaml` thay cho lệnh `docker run`.

### Home Assistant OS

Thêm repository này vào **Settings → Add-ons → Add-on Store → ⋮ → Repositories**, cài **NetPulse**, đặt `admin_password` rồi Start.
Trước khi phát hành, cần thay `OWNER` trong `homeassistant/netpulse/config.yaml` và `repository.yaml` bằng tài khoản GitHub của bạn.

### Tuỳ chọn

| Cờ / biến môi trường | Ý nghĩa |
|---|---|
| `-data` / `NETPULSE_DATA` | Thư mục dữ liệu (mặc định `./data`) |
| `-listen` / `NETPULSE_LISTEN` | Địa chỉ lắng nghe (mặc định `:8080`) |
| `-tls auto` / `NETPULSE_TLS=auto` | Bật HTTPS bằng chứng chỉ tự ký, lưu trong thư mục dữ liệu |
| `-tls-cert`, `-tls-key` | Dùng chứng chỉ riêng của bạn |
| `NETPULSE_ADMIN_PASSWORD` | Tạo admin ngay ở lần chạy đầu, không cần mã khởi tạo |
| `-trust-proxy` | Tin header `X-Forwarded-*` (chỉ bật khi đứng sau reverse proxy) |
| `-demo` | Tạo một router mô phỏng để dùng thử giao diện |

## Kết nối router

### Cách 1 – REST API (khuyên dùng)

Trên router (RouterOS 7.x), tạo user chỉ-đọc và chỉ cho phép đăng nhập từ máy chạy NetPulse:

```
/user group add name=netpulse policy=read,test,api,rest-api
/user add name=netpulse group=netpulse password="MatKhauManh" address=192.168.88.10/32
/certificate add name=netpulse-www common-name=router key-usage=tls-server
/certificate sign netpulse-www
/ip service set www-ssl certificate=netpulse-www disabled=no address=192.168.88.10/32
```

(`192.168.88.10` là IP máy chạy NetPulse. Nếu RouterOS báo không có policy `rest-api`, bỏ nó khỏi lệnh đầu.)

Sau đó vào **Router → Thêm router**, chọn "REST API", nhập IP, user và mật khẩu rồi bấm **Kiểm tra kết nối**.
Router ở chi nhánh khác có thể nối về bằng WireGuard. User này không đổi được cấu hình router.

### Cách 2 – Router tự gửi (push)

1. Thêm router với chế độ "Router tự gửi dữ liệu" rồi bấm **Script cài** để tải file `.rsc`.
2. Kiểm tra router cho phép fetch và scheduler (`/system device-mode print`). Nếu chưa: `/system device-mode update fetch=yes scheduler=yes` rồi xác nhận bằng nút reset hoặc ngắt điện.
3. Kéo file vào mục Files của WinBox và chạy `/import file-name=netpulse-<id>.rsc`.

Script chỉ có quyền `read,test` và chỉ tạo script cùng scheduler tên `netpulse`. Mỗi lần tải script mới, khoá cũ hết hiệu lực.
Router phải gọi tới được NetPulse. Nếu cần, đặt **Cài đặt → Địa chỉ NetPulse cho router**.

Gỡ khỏi router:

```
/system scheduler remove [find where name="netpulse"]
/system script remove [find where name="netpulse"]
```

## Vận hành

- **Quên mật khẩu**: dừng NetPulse rồi chạy `netpulse reset-password -data <thư mục> admin "MatKhauMoi123"`.
- **Sao lưu**: vào **Cài đặt → Tải bản sao lưu**. Để khôi phục, dừng NetPulse, chép file vào `<data>/netpulse.db` và giữ nguyên `secret.key`.
- **Dữ liệu**: `netpulse.db` (SQLite, WAL), `secret.key` (khoá AES-256-GCM để mã hoá mật khẩu router và Bot Token). Lịch sử được dọn tự động theo số ngày đặt trong Cài đặt.
- **Bảo mật**:
  - Mật khẩu băm bằng PBKDF2-SHA256 (300.000 vòng).
  - Cookie `HttpOnly` và `SameSite`. Mọi yêu cầu thay đổi phải có header riêng chống CSRF.
  - Có CSP chặt. Giao diện không dùng `innerHTML`, nên tên lấy từ router không chèn được mã.
  - Không lưu mật khẩu PPP hay bất kỳ trường bí mật nào từ router.

## Phát triển

```bash
go test -race ./...
go run . -demo -data ./data
```

```bash
VERSION=0.1.0 ./scripts/build.sh
```

Lệnh build tạo các gói cho mọi nền tảng trong `dist/`. Giao diện là HTML/CSS/JS thuần trong `internal/app/web`, được nhúng vào file chạy, không cần bước build.
