package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/gotd/td/telegram/dcs"
	"golang.org/x/net/proxy"
)

// wsProbeTimeout — сколько ждём websocket-рукопожатия при проверке транспорта.
const wsProbeTimeout = 15 * time.Second

// transportMode — каким транспортом идти до Telegram.
type transportMode int

const (
	// transportAuto: обычный TCP, а если прокси до дата-центров не доходит —
	// websocket.
	transportAuto transportMode = iota
	// transportPlain: только обычный MTProto поверх TCP по IP-адресам DC.
	transportPlain
	// transportWebsocket: только MTProto поверх websocket по доменным именам.
	transportWebsocket
)

// transportModeFromEnv читает TG_TRANSPORT (в том числе из .env).
func transportModeFromEnv() (transportMode, error) {
	switch v := strings.ToLower(strings.TrimSpace(os.Getenv("TG_TRANSPORT"))); v {
	case "", "auto":
		return transportAuto, nil
	case "plain", "tcp":
		return transportPlain, nil
	case "websocket", "ws", "wss":
		return transportWebsocket, nil
	default:
		return transportAuto, fmt.Errorf("TG_TRANSPORT=%q: допустимо auto, plain или websocket", v)
	}
}

// pickResolver выбирает транспорт и заодно проверяет, что он рабочий.
//
// Смысл проверки: сети Telegram у некоторых прокси попросту не в маршрутах —
// TCP до дата-центра по IP уходит в никуда. При этом доменные имена такой прокси
// резолвит сам и до Telegram доходит, поэтому websocket через него работает.
// Без проверки gotd молча крутил бы переподключения, а снаружи это выглядит как
// зависание.
func pickResolver(ctx context.Context, mode transportMode, viaProxy bool) (dcs.Resolver, error) {
	switch mode {
	case transportPlain:
		if viaProxy {
			if err := checkProxy(ctx, proxyConnectTimeout); err != nil {
				return nil, fmt.Errorf("%w\n   Если у прокси нет маршрута до сетей Telegram, помогает "+
					"TG_TRANSPORT=websocket", err)
			}
			log.Println("   Прокси отвечает, соединение с Telegram установлено")
		}
		return plainResolver(), nil

	case transportWebsocket:
		if err := checkWebsocket(ctx, wsProbeTimeout); err != nil {
			return nil, err
		}
		log.Println("   Websocket-транспорт отвечает")
		return websocketResolver(), nil
	}

	// transportAuto: без прокси проверять нечего — напрямую обычный TCP до
	// дата-центров и есть штатный путь.
	if !viaProxy {
		return plainResolver(), nil
	}

	plainErr := checkProxy(ctx, proxyConnectTimeout)
	if plainErr == nil {
		log.Println("   Прокси отвечает, соединение с Telegram установлено")
		return plainResolver(), nil
	}
	if ctx.Err() != nil {
		return nil, plainErr
	}

	log.Println("   Прокси не доходит до дата-центров Telegram по IP, пробую websocket...")
	if wsErr := checkWebsocket(ctx, wsProbeTimeout); wsErr != nil {
		return nil, fmt.Errorf("%w\n\n   Websocket-транспорт тоже не подошёл: %v", plainErr, wsErr)
	}

	log.Println("   Websocket работает — использую его (имена дата-центров резолвит прокси)")
	log.Println("   Закрепить выбор можно через TG_TRANSPORT=websocket")
	return websocketResolver(), nil
}

// plainResolver — штатный MTProto поверх TCP. proxy.Dial ходит через
// ALL_PROXY/NO_PROXY; без них — напрямую.
func plainResolver() dcs.Resolver {
	return dcs.Plain(dcs.PlainOptions{Dial: proxy.Dial})
}

// websocketResolver — MTProto поверх websocket (wss://<dc>.web.telegram.org).
// Адрес здесь доменный, поэтому имя разрешает прокси, а не мы: это и спасает,
// когда до IP-адресов дата-центров у прокси нет маршрута.
func websocketResolver() dcs.Resolver {
	return dcs.Websocket(dcs.WebsocketOptions{DialOptions: websocketDialOptions()})
}

func websocketDialOptions() *websocket.DialOptions {
	return &websocket.DialOptions{
		Subprotocols: []string{"binary"},
		// Timeout у клиента не выставляем: соединение живёт всю сессию.
		HTTPClient: &http.Client{
			Transport: &http.Transport{
				DialContext:         proxy.Dial,
				TLSHandshakeTimeout: 10 * time.Second,
			},
		},
	}
}

// checkWebsocket проверяет, что websocket-эндпоинт дата-центра доступен.
func checkWebsocket(ctx context.Context, timeout time.Duration) error {
	addr, ok := dcs.Prod().Domains[2]
	if !ok {
		return fmt.Errorf("не найден websocket-адрес дата-центра Telegram")
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	conn, resp, err := websocket.Dial(ctx, addr, websocketDialOptions())
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return fmt.Errorf("%s — %w", addr, err)
	}
	return conn.Close(websocket.StatusNormalClosure, "")
}
