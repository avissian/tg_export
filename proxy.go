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
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

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
		return "", fmt.Errorf("%s=%q: %w", envName, raw, err)
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
