package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/telegram/dcs"
	"golang.org/x/net/proxy"
)

// proxyConnectTimeout — сколько ждём соединения с Telegram через прокси
// при стартовой проверке.
const proxyConnectTimeout = 15 * time.Second

// proxyEnvVars — переменные с адресом прокси в порядке приоритета.
// Сам x/net/proxy читает только ALL_PROXY (и NO_PROXY для исключений),
// поэтому остальные варианты приводим к ALL_PROXY.
var proxyEnvVars = []string{
	"ALL_PROXY", "all_proxy",
	"HTTPS_PROXY", "https_proxy",
	"HTTP_PROXY", "http_proxy",
}

func init() {
	// x/net/proxy из коробки знает только socks5, а MTProto — это сырой TCP,
	// поэтому HTTP(S)-прокси подключаем через CONNECT-туннель.
	proxy.RegisterDialerType("http", newHTTPConnectDialer)
	proxy.RegisterDialerType("https", newHTTPConnectDialer)
}

// setupProxy нормализует адрес прокси из переменных окружения (включая
// подхваченные из .env) и кладёт его в ALL_PROXY, откуда его берёт proxy.Dial.
//
// Возвращает описание прокси для лога — или "", если прокси не задан.
// Ошибка означает, что адрес задан, но непригоден: молча ходить напрямую в
// таком случае нельзя, иначе IP утечёт мимо прокси.
func setupProxy() (string, error) {
	raw, envName := lookupProxyEnv()
	if raw == "" {
		return "", nil
	}

	u, err := parseProxyURL(raw)
	if err != nil {
		return "", fmt.Errorf("%s=%q: %w%s", envName, raw, err, specialCharsHint(raw))
	}

	// FromEnvironment при неразобранном адресе молча возвращает Direct,
	// поэтому проверяем адрес заранее и явно.
	if _, err := proxy.FromURL(u, proxy.Direct); err != nil {
		return "", fmt.Errorf("%s=%q: %w", envName, raw, err)
	}

	if err := os.Setenv("ALL_PROXY", u.String()); err != nil {
		return "", err
	}

	// Redacted() скрывает пароль, чтобы тот не утёк в лог.
	return fmt.Sprintf("%s (из %s)", u.Redacted(), envName), nil
}

// checkProxy пробует дойти через прокси до дата-центра Telegram.
//
// Без этой проверки неработающий прокси не даёт никакой обратной связи: gotd
// упирается в таймаут дозвона (35 c по умолчанию) и молча повторяет попытку —
// снаружи это выглядит как бесконечное зависание на старте.
func checkProxy(ctx context.Context, timeout time.Duration) error {
	addrs, err := telegramProbeAddrs()
	if err != nil {
		return err
	}

	// Адреса пробуем разом: последовательный перебор занимал бы timeout на
	// каждый, а нам достаточно любого дозвонившегося.
	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	type result struct {
		addr string
		conn net.Conn
		err  error
	}
	results := make(chan result, len(addrs))
	for _, addr := range addrs {
		go func(addr string) {
			conn, err := proxy.Dial(attemptCtx, "tcp", addr)
			results <- result{addr: addr, conn: conn, err: err}
		}(addr)
	}

	var failures []string
	var ok bool
	for range addrs {
		r := <-results
		switch {
		case r.err != nil:
			failures = append(failures, fmt.Sprintf("   • %s — %v", r.addr, r.err))
		case ok: // уже есть рабочее соединение, это закрываем
			r.conn.Close()
		default:
			ok = true
			r.conn.Close()
		}
	}
	if ok {
		return nil
	}
	if ctx.Err() != nil { // Ctrl+C
		return ctx.Err()
	}
	sort.Strings(failures)

	return fmt.Errorf("прокси не смог подключиться ни к одному дата-центру Telegram:\n%s\n"+
		"   Для MTProto нужен SOCKS5 или HTTP-прокси с разрешённым CONNECT на порт 443.\n"+
		"   Проверить прокси отдельно: ALL_PROXY=<тот же адрес> curl -sS https://ifconfig.me\n"+
		"   Подробный лог: запустите с ключом -v\n"+
		"   Отключить прокси: уберите ALL_PROXY/HTTPS_PROXY/HTTP_PROXY из .env и окружения",
		strings.Join(failures, "\n"))
}

// telegramProbeAddrs возвращает адреса дата-центров Telegram для проверки связи:
// сначала Static (они и предназначены для работы через прокси), потом обычные.
// Один недоступный адрес ещё не значит, что прокси нерабочий, поэтому пробуем
// несколько и по разным дата-центрам.
func telegramProbeAddrs() ([]string, error) {
	var static, regular []string
	seen := make(map[string]bool)

	for _, dc := range []int{2, 4} {
		for _, o := range dcs.FindPrimaryDCs(dcs.Prod().Options, dc, false) {
			// IPv6 пропускаем: далеко не каждый прокси его умеет, и отказ
			// в IPv6 ничего не говорит о работоспособности прокси.
			if o.MediaOnly || o.CDN || o.TCPObfuscatedOnly || o.Ipv6 {
				continue
			}
			addr := net.JoinHostPort(o.IPAddress, strconv.Itoa(o.Port))
			if seen[addr] {
				continue
			}
			seen[addr] = true
			if o.Static {
				static = append(static, addr)
			} else {
				regular = append(regular, addr)
			}
		}
	}

	addrs := append(static, regular...)
	if len(addrs) == 0 {
		return nil, fmt.Errorf("не найден адрес дата-центра Telegram для проверки прокси")
	}
	if len(addrs) > 4 {
		addrs = addrs[:4]
	}
	return addrs, nil
}

// specialCharsHint подсказывает про спецсимволы в пароле, если они похоже и
// сломали разбор. В URL пароль — это часть адреса: `#`, `@`, `/`, `?`, `:`
// нужно писать в процентной кодировке, иначе адрес обрывается на первом же
// таком символе. Плюс `#` в .env без кавычек godotenv считает началом
// комментария и отрезает остаток строки.
func specialCharsHint(raw string) string {
	// Отрезаем схему: `//` и `:` в ней законны.
	rest := raw
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	// Хост от userinfo отделяет последняя `@`; спецсимволы ищем до неё.
	userinfo := rest
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		userinfo = rest[:i]
	} else {
		return ""
	}

	var found []string
	for _, c := range []struct {
		ch, enc string
	}{
		{"#", "%23"}, {"@", "%40"}, {"/", "%2F"}, {"?", "%3F"},
	} {
		if strings.Contains(userinfo, c.ch) {
			found = append(found, c.ch+" → "+c.enc)
		}
	}
	// Одна `:` разделяет логин и пароль, лишние — уже спецсимвол в пароле.
	if strings.Count(userinfo, ":") > 1 {
		found = append(found, ": → %3A")
	}
	if len(found) == 0 {
		return ""
	}

	return fmt.Sprintf("\n   Похоже, в логине или пароле есть спецсимволы — их нужно записать в\n"+
		"   процентной кодировке: %s\n"+
		"   А значение с `#` в .env ещё и берите в кавычки, иначе остаток строки\n"+
		"   будет отброшен как комментарий.", strings.Join(found, ", "))
}

func lookupProxyEnv() (value, name string) {
	for _, n := range proxyEnvVars {
		if v := strings.TrimSpace(os.Getenv(n)); v != "" {
			return v, n
		}
	}
	return "", ""
}

// parseProxyURL разбирает адрес прокси и достраивает пропущенное.
// Адрес без схемы (`127.0.0.1:1080`) считается socks5.
func parseProxyURL(raw string) (*url.URL, error) {
	if !strings.Contains(raw, "://") {
		raw = "socks5://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("не удалось разобрать адрес: %w", err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("не указан хост")
	}
	if u.Port() == "" {
		port, ok := defaultProxyPort(u.Scheme)
		if !ok {
			return nil, fmt.Errorf("схема %q не поддерживается (нужно socks5, socks5h, http или https)", u.Scheme)
		}
		u.Host = net.JoinHostPort(u.Host, port)
	}
	return u, nil
}

func defaultProxyPort(scheme string) (string, bool) {
	switch strings.ToLower(scheme) {
	case "socks5", "socks5h":
		return "1080", true
	case "http":
		return "80", true
	case "https":
		return "443", true
	}
	return "", false
}

// httpConnectDialer открывает TCP-туннель до Telegram через HTTP(S)-прокси
// методом CONNECT. Реализует proxy.Dialer и proxy.ContextDialer.
type httpConnectDialer struct {
	addr     string // host:port прокси
	useTLS   bool   // схема https — до прокси идём по TLS
	hostname string // имя для проверки сертификата прокси
	auth     string // значение Proxy-Authorization или ""
	forward  proxy.Dialer
}

func newHTTPConnectDialer(u *url.URL, forward proxy.Dialer) (proxy.Dialer, error) {
	d := &httpConnectDialer{
		addr:     u.Host,
		useTLS:   strings.EqualFold(u.Scheme, "https"),
		hostname: u.Hostname(),
		forward:  forward,
	}
	if u.User != nil {
		pwd, _ := u.User.Password()
		d.auth = "Basic " + base64.StdEncoding.EncodeToString([]byte(u.User.Username()+":"+pwd))
	}
	return d, nil
}

func (d *httpConnectDialer) Dial(network, addr string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, addr)
}

func (d *httpConnectDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return nil, fmt.Errorf("http-прокси: сеть %q не поддерживается", network)
	}

	conn, err := d.dialProxy(ctx, network)
	if err != nil {
		return nil, fmt.Errorf("подключение к прокси %s: %w", d.addr, err)
	}

	tunnelOK := false
	defer func() {
		if !tunnelOK {
			conn.Close()
		}
	}()

	// Дедлайн из контекста накрывает рукопожатие с прокси, иначе зависший
	// прокси подвесит и нас.
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return nil, err
		}
	}

	if d.useTLS {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: d.hostname})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return nil, fmt.Errorf("TLS-рукопожатие с прокси %s: %w", d.addr, err)
		}
		conn = tlsConn
	}

	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: addr},
		Host:   addr,
		Header: make(http.Header),
	}
	if d.auth != "" {
		req.Header.Set("Proxy-Authorization", d.auth)
	}
	if err := req.Write(conn); err != nil {
		return nil, fmt.Errorf("CONNECT %s: %w", addr, err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		return nil, fmt.Errorf("ответ прокси на CONNECT %s: %w", addr, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("прокси отказал в CONNECT %s: %s", addr, resp.Status)
	}

	// Туннель открыт, дальше по нему идёт MTProto — снимаем дедлайн.
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	tunnelOK = true

	// Читаем через br: прокси мог прислать первые байты Telegram сразу за
	// заголовками, и они уже лежат в буфере.
	return &bufferedConn{Conn: conn, reader: br}, nil
}

func (d *httpConnectDialer) dialProxy(ctx context.Context, network string) (net.Conn, error) {
	if cd, ok := d.forward.(proxy.ContextDialer); ok {
		return cd.DialContext(ctx, network, d.addr)
	}
	return d.forward.Dial(network, d.addr)
}

// bufferedConn — соединение, чтение которого начинается с уже вычитанного буфера.
type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }
