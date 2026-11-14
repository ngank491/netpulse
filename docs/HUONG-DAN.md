# Hướng dẫn sử dụng NetPulse

Tài liệu này hướng dẫn từng bước:

1. [Đăng nhập lần đầu](#1-đăng-nhập-lần-đầu)
2. [Thêm router MikroTik](#2-thêm-router-mikrotik) – cách A (REST API, khuyên dùng) và cách B (router tự gửi)
3. [Theo dõi thiết bị và đặt tên client](#3-theo-dõi-thiết-bị-và-đặt-tên-client)
4. [Nhận cảnh báo qua Telegram](#4-nhận-cảnh-báo-qua-telegram)
5. [Nhận cảnh báo qua Zalo](#5-nhận-cảnh-báo-qua-zalo)
6. [NetPulse cảnh báo những gì](#6-netpulse-cảnh-báo-những-gì)
7. [Trợ lý AI kiểm tra bảo mật và hiệu suất](#7-trợ-lý-ai-kiểm-tra-bảo-mật-và-hiệu-suất)
8. [Xử lý sự cố](#8-xử-lý-sự-cố)

Trong hướng dẫn, `192.168.1.4` là IP máy chạy NetPulse và `192.168.1.1` là router. Hãy thay bằng địa chỉ thực tế của bạn.

---

## 1. Đăng nhập lần đầu

1. Mở trình duyệt, vào `http://192.168.1.4:8090` (hoặc cổng bạn đã chọn).
2. Ở lần chạy đầu, trang hiện **Khởi tạo NetPulse** và hỏi **mã khởi tạo**. Mã có ở một trong hai nơi:
   - cửa sổ hoặc log của NetPulse (dòng `LẦN CHẠY ĐẦU – … nhập mã: XXXX-XXXX`);
   - file `setup-code.txt` trong thư mục dữ liệu, ví dụ:
     ```bash
     cat ~/.local/share/netpulse/setup-code.txt
     ```
3. Nhập mã, đặt tên đăng nhập và mật khẩu quản trị (ít nhất 10 ký tự). Mã chỉ dùng được một lần.

> Nếu từ máy khác không mở được trang, kiểm tra tường lửa của server. Ví dụ với UFW, chỉ mở cho mạng LAN:
> ```bash
> sudo ufw allow from 192.168.1.0/24 to any port 8090 proto tcp comment 'NetPulse LAN'
> ```

---

## 2. Thêm router MikroTik

Có hai cách. Chọn **cách A** nếu NetPulse kết nối được tới router (cùng LAN, hoặc qua VPN/WireGuard). Chọn **cách B** nếu router ở xa, sau NAT, không có VPN.

| | Cách A – REST API | Cách B – Router tự gửi |
|---|---|---|
| RouterOS | 7.1 trở lên | 7.17 trở lên |
| Cài gì trên router | 1 user chỉ-đọc + bật HTTPS | 1 script + 1 scheduler |
| Đổi device-mode / khởi động lại | Không | Có thể phải bật `fetch`, `scheduler` |
| Hướng kết nối | NetPulse → router | Router → NetPulse |

### Cách A – REST API (khuyên dùng)

#### Bước A1. Tạo user chỉ-đọc trên router

Mở **WinBox → New Terminal**. Chạy **từng lệnh một**; nếu dán nhiều dòng cùng lúc, terminal có thể ngắt dòng làm hỏng lệnh.

```
/user group add name=netpulse policy=read,test,api,rest-api
```
```
/user add name=netpulse group=netpulse address=192.168.1.4/32 password="DatMatKhauManh"
```

- `read,test` nghĩa là user chỉ đọc được dữ liệu và chạy ping, không thay đổi được cấu hình.
- `address=192.168.1.4/32` nghĩa là user chỉ đăng nhập được từ máy NetPulse.
- Nếu RouterOS báo lỗi ở `rest-api` (bản cũ không có quyền này), chạy lại lệnh đầu mà bỏ `,rest-api`.

#### Bước A2. Bật HTTPS (www-ssl) với chứng chỉ riêng

RouterOS cần một CA nội bộ để ký chứng chỉ HTTPS. Chạy lần lượt:

```
/certificate add name=netpulse-ca common-name=netpulse-ca key-usage=key-cert-sign,crl-sign days-valid=3650
```
```
/certificate sign netpulse-ca
```
```
/certificate add name=netpulse-www common-name=192.168.1.1 days-valid=3650
```
```
/certificate sign netpulse-www ca=netpulse-ca
```

Kiểm tra trước khi bật dịch vụ:

```
/certificate print
```

`netpulse-ca` phải có cờ **K** và **A**; `netpulse-www` phải có cờ **K** và **I**. Khi đã đúng, bật dịch vụ:

```
/ip service set www-ssl certificate=netpulse-www disabled=no address=192.168.1.4/32
```

> Nếu bạn còn dùng WebFig qua HTTPS từ máy khác, thêm IP máy đó vào `address=`, ví dụ `address=192.168.1.4/32,192.168.1.3/32`.
>
> **Không muốn làm chứng chỉ?** Có thể dùng HTTP trong LAN tin cậy: `/ip service set www disabled=no address=192.168.1.4/32`, rồi bỏ chọn **Dùng HTTPS** ở bước A3. Đổi lại, mật khẩu user `netpulse` truyền dạng không mã hoá trong LAN.

Nếu router có firewall chặn truy cập vào router (chain `input`), thêm:

```
/ip firewall filter add chain=input src-address=192.168.1.4 protocol=tcp dst-port=443 action=accept place-before=0 comment=NetPulse
```

#### Bước A3. Thêm router trong NetPulse

1. Vào **Router → + Thêm router**.
2. Điền:

| Ô | Giá trị |
|---|---|
| Tên hiển thị | ví dụ "Văn phòng chính" |
| Cách kết nối | NetPulse đọc router qua REST API |
| Địa chỉ router | `192.168.1.1` |
| Cổng | để trống (mặc định 443 với HTTPS, 80 với HTTP) |
| User router / Mật khẩu | `netpulse` / mật khẩu ở bước A1 |
| Dùng HTTPS | ✔ |
| Kiểm tra chứng chỉ | bỏ chọn (chứng chỉ tự ký) |

3. Bấm **Kiểm tra kết nối**. Nếu thành công, form hiện tên, model, phiên bản RouterOS và số interface. Ô WAN sẽ gợi ý tên interface.
4. Phần **WAN cần theo dõi**: bấm **+ Thêm WAN** cho mỗi đường Internet.
   - Với PPPoE, chọn interface `pppoe-out…`, không chọn cổng ether bên dưới nó.
   - Đặt nhãn là tên nhà mạng (VNPT, Viettel, FPT…).
   - NetPulse sẽ ping **IP kiểm tra Internet** (mặc định `8.8.8.8`) qua đúng interface đó để biết đường đó còn Internet không.
5. (Tuỳ chọn) **Chat ID Telegram / Zalo riêng cho router này**: cảnh báo của router này còn gửi thêm vào nhóm đó. Xem mục 4 và 5.
6. Bấm **Thêm router**. Khoảng 1 phút sau, trang Tổng quan có dữ liệu.

### Cách B – Router tự gửi dữ liệu (push)

1. Vào **Cài đặt → Địa chỉ NetPulse cho router**, nhập địa chỉ router gọi tới được, ví dụ `http://113.x.x.x:8090` hoặc `http://10.0.0.254:8090` (qua WireGuard). Lưu cài đặt.
2. Vào **Router → + Thêm router**, chọn **Router tự gửi dữ liệu**, đặt tên và WAN, rồi lưu.
3. Bấm **Script cài** → **Tải script**. Mỗi lần tải, NetPulse tạo khoá mới và khoá cũ hết hiệu lực.
4. Trên router, kiểm tra device-mode:
   ```
   /system device-mode print
   ```
   Nếu `fetch` hoặc `scheduler` đang tắt:
   ```
   /system device-mode update fetch=yes scheduler=yes
   ```
   rồi bấm nút reset (hoặc ngắt điện) trong vòng 5 phút để xác nhận.
5. Kéo file `netpulse-<số>.rsc` vào mục **Files** của WinBox, rồi chạy:
   ```
   /import file-name=netpulse-<số>.rsc
   ```
6. Sau khoảng 1 phút, router hiện **Hoạt động**.

Gỡ khỏi router:
```
/system scheduler remove [find where name="netpulse"]
/system script remove [find where name="netpulse"]
```

---

## 3. Theo dõi thiết bị và đặt tên client

### Thiết bị theo dõi (ping định kỳ)

Dùng cho camera, switch, AP, NAS, máy in… mà bạn muốn được báo khi mất kết nối.

1. Mở router → tab **Thiết bị** → **+ Thêm thiết bị**, hoặc **Thêm từ DHCP/neighbor** để chọn nhiều thiết bị một lúc.
2. Chọn **Nối với** để vẽ đúng sơ đồ (tab **Sơ đồ**, kéo thả rồi bấm **Lưu bố cục**).
3. Bật **Báo khi mất kết nối** và chọn **Khi vẫn chưa khắc phục** (nhắc lại sau 15 phút đến 2 giờ).

Router ping 2 gói mỗi 30 giây. Thiết bị chỉ bị báo mất kết nối sau **2 lần liên tiếp** không phản hồi.

### Tab Client

Tab **Client** liệt kê mọi máy router thấy (DHCP, ARP, bridge):

- **Trạng thái**: *Hoạt động* nếu router thấy gói tin từ máy đó trong 5 phút gần nhất; *Ngừng* nếu không.
- **Thời gian hoạt động / dừng**: đang hoạt động bao lâu (từ lúc nào); hoặc dừng lúc nào, đã bao lâu, và phiên hoạt động trước đó dài bao lâu.
- **Tên máy** lấy theo thứ tự: tên tự đặt → thiết bị theo dõi → DHCP → ghi chú trên router → DNS tĩnh → neighbor → NetBIOS / mDNS / DNS ngược (NetPulse hỏi trực tiếp máy, cần cùng mạng). Nếu vẫn không có, hiện **hãng sản xuất** theo MAC.
- Máy ghi **MAC ngẫu nhiên** là điện thoại hoặc laptop bật "địa chỉ Wi-Fi riêng tư". Chúng thường không công bố tên. Bấm **Đặt tên** để gán tên. Tên gắn với MAC nên vẫn đúng khi IP thay đổi.
- Muốn router tự có tên cho máy đặt IP tĩnh, có thể thêm DNS tĩnh trên router: `/ip dns static add name=camera-cong address=192.168.1.50`.

---

## 4. Nhận cảnh báo qua Telegram

### Bước T1. Tạo bot

1. Trong Telegram, tìm **@BotFather** → gửi `/newbot`.
2. Đặt tên hiển thị, rồi đặt username kết thúc bằng `bot` (ví dụ `congty_netpulse_bot`).
3. BotFather trả về **token** dạng `123456789:AAH…`. Giữ bí mật token này.

### Bước T2. Lưu token trong NetPulse

1. Vào **Cài đặt → Cảnh báo Telegram**, dán token vào **Bot Token**.
2. Bấm **Lưu cài đặt**. Huy hiệu chuyển thành **Đã có Bot Token**.

### Bước T3. Lấy Chat ID

- **Nhận cá nhân:** mở bot vừa tạo trong Telegram và bấm **Start** (gửi `/start`). Bot trả lời Chat ID.
- **Nhận trong nhóm:** thêm bot vào nhóm rồi gõ `/id` trong nhóm. Chat ID của nhóm là số âm, ví dụ `-1001234567890`.

Sau vài giây, Chat ID cũng hiện ở bảng **Tin nhắn gần đây gửi tới bot** trong Cài đặt. Bấm **Dùng làm Chat ID quản trị** để điền tự động.

### Bước T4. Lưu và gửi thử

1. Bấm **Lưu cài đặt**, rồi **Gửi tin thử**. Điện thoại phải nhận được "✅ NetPulse đã kết nối Telegram thành công."
2. Muốn từng router báo vào một nhóm riêng: vào **Router → Sửa** và nhập Chat ID nhóm vào **Chat ID Telegram riêng cho router này**.

Gõ `/status` cho bot bất cứ lúc nào để xem tình trạng. Chat quản trị xem được mọi router; nhóm riêng chỉ xem được router của nhóm đó.

---

## 5. Nhận cảnh báo qua Zalo

NetPulse dùng **Zalo Bot Platform** (bot chính thức của Zalo, miễn phí, không cần Official Account).

### Bước Z1. Tạo bot Zalo

1. Mở ứng dụng **Zalo** trên điện thoại, tìm Official Account **Zalo Bot Manager**.
2. Trong khung chat, mở menu và chọn **Tạo bot**. Ứng dụng **Zalo Bot Creator** mở ra.
3. Đặt tên bot **bắt đầu bằng chữ "Bot"** (ví dụ "Bot NetPulse Công ty"), điền thông tin và bấm **Tạo Bot**.
4. Zalo gửi thông tin bot kèm **Bot Token** (dạng `12345678:abc-xyz…`) vào tài khoản của bạn. Giữ bí mật token này. Bạn có thể đặt lại token trong Zalo Bot Creator nếu bị lộ.

### Bước Z2. Lưu token trong NetPulse

1. Vào **Cài đặt → Cảnh báo Zalo**, dán token vào **Bot Token**.
2. Bấm **Lưu cài đặt**.

### Bước Z3. Lấy Chat ID

1. Trong Zalo, mở cuộc trò chuyện với bot vừa tạo và nhắn **`id`**.
2. Bot trả lời "Chat ID của cuộc trò chuyện này: …". Chat ID cũng hiện ở bảng **Tin nhắn gần đây gửi tới bot** trong Cài đặt. Bấm **Dùng làm Chat ID quản trị**.
3. Mỗi người muốn nhận cảnh báo đều phải **nhắn cho bot ít nhất một lần** để có Chat ID riêng. Bot chỉ gửi được cho người đã trò chuyện với nó.

### Bước Z4. Lưu và gửi thử

1. Bấm **Lưu cài đặt**, rồi **Gửi tin thử** ở khung Zalo.
2. Muốn một router báo thêm cho người khác: vào **Router → Sửa** và nhập Chat ID Zalo của người đó vào **Chat ID Zalo riêng cho router này**.

Lưu ý về Zalo:

- Mỗi tin Zalo tối đa 2000 ký tự. NetPulse tự chia và gộp tin cho phù hợp.
- Tài liệu Zalo ghi việc bot hoạt động trong **nhóm** đang ở mức thử nghiệm (Beta). Nên dùng chat cá nhân với bot. Nếu muốn dùng nhóm, hãy thử bằng **Gửi tin thử** trước.
- Nhắn **`status`** cho bot để xem tình trạng mạng.

Có thể bật **cả Telegram và Zalo** cùng lúc: mỗi cảnh báo được gửi tới mọi kênh đã cấu hình.

---

## 6. NetPulse cảnh báo những gì

| Sự kiện | Ghi vào Sự kiện | Gửi Telegram/Zalo |
|---|---|---|
| Mất / có lại kết nối tới router | ✔ | ✔ |
| WAN mất link / mất Internet / phục hồi | ✔ | ✔ |
| Thiết bị theo dõi mất kết nối / phục hồi | ✔ | ✔ (nếu bật cho thiết bị đó) |
| Router khởi động lại | ✔ | ✔ |
| RouterOS đổi phiên bản | ✔ | ✔ |
| Thiết bị lạ lần đầu vào mạng | ✔ | tuỳ chọn trong Cài đặt |
| Cổng (ether/sfp/vlan/bridge/wlan) mất hoặc có link | ✔ | – |
| IP public của WAN thay đổi | ✔ | – |
| NetPulse khởi động lại (kèm thời gian gián đoạn) | ✔ | – |

- **Chống báo nhầm:** WAN và thiết bị chỉ bị báo lỗi sau 2 lần kiểm tra liên tiếp thất bại; router chỉ bị báo mất kết nối sau 3 lần.
- **Nhắc lại:** nếu lỗi chưa khắc phục, NetPulse nhắc lại theo chu kỳ trong Cài đặt (WAN/router) hoặc trong từng thiết bị.
- **Bản tin hằng ngày:** gửi lúc 08:00 (đổi được trong Cài đặt), gồm trạng thái từng router, WAN và thiết bị đang lỗi.
- **Không mất tin:** cảnh báo được ghi vào database trước khi gửi. Khi mất Internet, NetPulse tự gửi lại khi có mạng (giữ tối đa 24 giờ). Nhiều cảnh báo xảy ra cùng lúc được gộp thành một tin.

Trang **Sự kiện** có ô tìm kiếm và bộ lọc theo mức (Cảnh báo / Thông tin).

---

## 7. Trợ lý AI kiểm tra bảo mật và hiệu suất

Tab **Kiểm tra AI** trong trang router (chỉ quản trị viên chạy được; người xem chỉ đọc báo cáo).

### Cách hoạt động

1. NetPulse **đọc cấu hình router một lần, ở chế độ chỉ đọc**: firewall, NAT, dịch vụ quản trị, tài khoản, DNS, UPnP/SOCKS/proxy, Wi-Fi, scheduler/script, log, phiên bản RouterOS, bộ đếm lỗi interface, connection tracking… Không có lệnh nào thay đổi router.
2. **Bộ kiểm tra có sẵn** (khoảng 30 quy tắc, miễn phí, chạy ngay trên máy NetPulse) chấm điểm và liệt kê vấn đề kèm lệnh khắc phục. Ví dụ: chain input không chặn WAN, Telnet/HTTP bật, DNS mở ra Internet, mở cổng RDP/SMB ra ngoài, dò mật khẩu trong log, script tải mã lạ, RouterOS cũ, thiếu FastTrack, cổng nhiều lỗi gói, CPU/RAM cao…
3. Nếu bật **trợ lý AI (Claude)**, agent tự đọc thêm các mục cần thiết, đối chiếu lịch sử CPU/RAM/lưu lượng/sự kiện trong NetPulse, loại cảnh báo sai, tìm thêm vấn đề quy tắc bỏ sót (thứ tự rule, NAT, tài khoản lạ…). Sau đó agent viết **tổng quan, danh sách vấn đề theo mức độ và lệnh RouterOS khắc phục theo thứ tự an toàn**.

Router ở **chế độ push** chỉ được phân tích bằng dữ liệu giám sát. Muốn kiểm tra cấu hình bảo mật đầy đủ, dùng chế độ REST API.

### Bật trợ lý AI

1. Tạo API key tại **console.anthropic.com → API Keys**. Tài khoản cần có credit.
2. Vào **Cài đặt → Trợ lý AI kiểm tra router (Claude)**:
   - dán **Anthropic API key**;
   - chọn **Mô hình**: *Claude Opus 5.5* (phân tích kỹ nhất, mặc định), *Sonnet 5.5* (nhanh, rẻ hơn) hoặc *Haiku 5.5* (rẻ nhất);
   - đánh dấu **Cho phép gửi cấu hình router … tới Anthropic**;
   - giữ **Che IP public và địa chỉ MAC** (khuyên dùng);
   - (tuỳ chọn) **Tự động kiểm tra hằng tuần**: sáng Chủ nhật, NetPulse kiểm tra mọi router và gửi tóm tắt qua Telegram/Zalo.
3. **Lưu cài đặt**, rồi mở router → **Kiểm tra AI** → **Chạy kiểm tra**. Mỗi lần mất khoảng 1–3 phút. Trong lúc chạy, trang hiện agent đang xem mục nào.

### Dữ liệu gửi đi và chi phí

- **Không bao giờ gửi:** mật khẩu, khoá riêng, PSK Wi-Fi, khoá WireGuard. Các trường này không được đọc từ router; mật khẩu nằm trong nội dung script cũng bị che (`password=***`).
- Khi bật **Che IP public và MAC**, NetPulse thay các giá trị này bằng mã (`PUBLIC-IP-1`, `MAC-3`) trước khi gửi, rồi đổi lại thành giá trị thật trong báo cáo trên máy bạn.
- Mỗi báo cáo ghi rõ **agent đã xem những mục nào**, số token và **chi phí ước tính** (USD).
- NetPulse dùng tính năng dự phòng của Anthropic: nếu mô hình chính từ chối một yêu cầu, một mô hình khác sẽ xử lý trong cùng lần gọi.
- Không bật AI thì báo cáo vẫn có đầy đủ phần của bộ kiểm tra có sẵn.

### Đọc báo cáo

- **Điểm Bảo mật** và **Hiệu suất & ổn định** (0–100): từ 80 trở lên là tốt, 50–79 cần xử lý, dưới 50 là rủi ro cao.
- **Vấn đề và cách khắc phục:** sắp xếp theo mức độ, mỗi mục có dữ liệu chứng minh và khối lệnh có nút **Sao chép**.
- **Trước khi dán lệnh đổi firewall, dịch vụ hoặc tài khoản:** bật **Safe Mode** trong Terminal của WinBox (**Ctrl+X**). Nếu lỡ tự khoá mình, chỉ cần ngắt kết nối và router tự hoàn tác. Nên chạy từng lệnh một và kiểm tra lại.
- Ô chọn ở đầu tab cho xem lại các lần kiểm tra trước để so sánh trước và sau khi sửa.

---

## 8. Xử lý sự cố

| Thông báo / hiện tượng | Nguyên nhân | Cách sửa |
|---|---|---|
| Không mở được trang NetPulse từ máy khác | Tường lửa trên server chặn cổng | Mở cổng (xem mục 1) |
| `router từ chối HTTPS: www-ssl chưa có chứng chỉ dùng được` | Chứng chỉ chưa ký hoặc www-ssl chưa gắn chứng chỉ | Làm lại bước A2, kiểm tra cờ **K** |
| `failure: CA not found` khi `/certificate sign` | RouterOS cần CA để ký chứng chỉ HTTPS | Tạo `netpulse-ca` trước (bước A2) |
| `input does not match any value of certificate` | Chứng chỉ chưa tồn tại hoặc chưa ký | `/certificate print` để kiểm tra tên và cờ |
| `router từ chối kết nối (dịch vụ www/www-ssl chưa bật hoặc sai cổng)` | Dịch vụ tắt hoặc `address=` không có IP NetPulse | `/ip service print`, bật lại dịch vụ |
| `router không phản hồi (timeout)` | Firewall `input` của router chặn | Thêm rule ở cuối bước A2 |
| `sai user hoặc mật khẩu router (401)` | Sai mật khẩu, hoặc user bị giới hạn IP khác | Kiểm tra `/user print`, đặt lại mật khẩu |
| `user router thiếu quyền` | Thiếu policy `api`/`rest-api`/`test` | Sửa group ở bước A1 |
| WAN báo "Không thấy" | Tên interface WAN không đúng | Router → Sửa → chọn lại theo gợi ý |
| Telegram/Zalo không nhận tin | Chưa lưu token, sai Chat ID, hoặc chưa nhắn bot trước | Xem lỗi đỏ cạnh tiêu đề kênh trong Cài đặt; nhắn bot rồi **Gửi tin thử** |
| `Bot Token sai hoặc đã bị thu hồi` | Token bị đặt lại | Lấy token mới, dán lại |
| Client hiện "MAC ngẫu nhiên" | Điện thoại/laptop dùng địa chỉ riêng tư | Bấm **Đặt tên** |
| `Trợ lý AI lỗi: API key Anthropic không hợp lệ` | Sai key hoặc key đã bị thu hồi | Tạo key mới, dán lại trong Cài đặt |
| `… vượt giới hạn tần suất hoặc hết hạn mức …` | Hết credit hoặc gọi quá nhiều | Nạp credit tại console.anthropic.com, thử lại sau |
| Báo cáo ghi "Router ở chế độ push…" | Router không đọc được qua REST API | Chuyển router sang cách A để kiểm tra cấu hình đầy đủ |
| Quên mật khẩu NetPulse | – | Dừng NetPulse, chạy `netpulse reset-password -data <thư mục dữ liệu> admin "MatKhauMoi123"` |

Nếu vẫn không được, xem log của NetPulse. Với dịch vụ systemd của user:

```bash
systemctl --user status netpulse
```
