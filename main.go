package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/auth/qrlogin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/joho/godotenv"
	"go.uber.org/zap"
	"golang.org/x/term"
	"rsc.io/qr"
)

// terminalAuth реализует auth.UserAuthenticator
type terminalAuth struct {
	phone string
}

func (a terminalAuth) Phone(_ context.Context) (string, error) { return a.phone, nil }

func (a terminalAuth) Password(_ context.Context) (string, error) {
	fmt.Print("Введите пароль 2FA: ")
	pwd, err := readPassword()
	if err != nil {
		return "", err
	}
	fmt.Println()
	return pwd, nil
}

func (a terminalAuth) Code(_ context.Context, sentCode *tg.AuthSentCode) (string, error) {
	fmt.Println()
	fmt.Println("═══════════════════════════════════════════")
	switch t := sentCode.Type.(type) {
	case *tg.AuthSentCodeTypeApp:
		fmt.Printf("📱 Код отправлен в приложение Telegram (%d цифр)\n", t.Length)
	case *tg.AuthSentCodeTypeSMS:
		fmt.Printf("📩 Код отправлен по SMS на %s (%d цифр)\n", a.phone, t.Length)
	case *tg.AuthSentCodeTypeCall:
		fmt.Println("📞 Код будет продиктован по звонку")
	case *tg.AuthSentCodeTypeMissedCall:
		fmt.Printf("📞 Пропущенный звонок (последние %d цифр)\n", t.Length)
	default:
		fmt.Printf("📨 Код отправлен (%T)\n", sentCode.Type)
	}
	fmt.Println("═══════════════════════════════════════════")
	fmt.Print("Введите код: ")
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Scan()
	code := strings.TrimSpace(scanner.Text())
	if code == "" {
		return "", fmt.Errorf("код не введён")
	}
	return code, nil
}

func (a terminalAuth) AcceptTermsOfService(_ context.Context, _ tg.HelpTermsOfService) error {
	return nil
}

func (a terminalAuth) SignUp(_ context.Context) (auth.UserInfo, error) {
	return auth.UserInfo{}, fmt.Errorf("регистрация не поддерживается")
}

// ChatItem описывает выбранный диалог
type ChatItem struct {
	Title     string
	InputPeer tg.InputPeerClass
	Type      string // "user", "chat", "channel"
}

// chatsPerPage — сколько диалогов показывать на одной странице списка
const chatsPerPage = 25

// errAborted возвращается, когда пользователь сам прервал выбор
var errAborted = errors.New("выбор отменён")

func main() {
	_ = godotenv.Load()

	appID, _ := strconv.Atoi(os.Getenv("APP_ID"))
	appHash := os.Getenv("APP_HASH")
	phone := os.Getenv("PHONE")

	if appID == 0 || appHash == "" {
		log.Fatal("❌ Укажите APP_ID и APP_HASH в .env файле")
	}

	useCodeAuth := false
	for _, arg := range os.Args[1:] {
		if arg == "--code-auth" {
			useCodeAuth = true
		}
	}

	if useCodeAuth && phone == "" {
		log.Fatal("❌ Для авторизации по коду укажите PHONE в .env")
	}

	appCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	dispatcher := tg.NewUpdateDispatcher()
	client := telegram.NewClient(appID, appHash, telegram.Options{
		SessionStorage: &session.FileStorage{Path: "session.json"},
		UpdateHandler:  dispatcher,
		Logger:         zap.NewNop(),
	})

	if err := client.Run(appCtx, func(ctx context.Context) error {
		status, err := client.Auth().Status(ctx)
		if err != nil {
			return fmt.Errorf("проверка авторизации: %w", err)
		}

		if !status.Authorized {
			if useCodeAuth {
				log.Println("🔑 Авторизация по коду...")
				flow := auth.NewFlow(terminalAuth{phone: phone}, auth.SendCodeOptions{})
				if err := client.Auth().IfNecessary(ctx, flow); err != nil {
					return fmt.Errorf("ошибка авторизации: %w", err)
				}
			} else {
				log.Println("📱 Авторизация по QR-коду...")
				log.Println("   Telegram → Настройки → Устройства → Подключить устройство")
				if err := authByQR(ctx, client, dispatcher, appID, appHash); err != nil {
					return fmt.Errorf("ошибка QR-авторизации: %w", err)
				}
			}
		}

		log.Println("✅ Авторизован")

		api := client.API()

		chats, err := listChats(ctx, api)
		if err != nil {
			return fmt.Errorf("загрузка диалогов: %w", err)
		}
		if len(chats) == 0 {
			return fmt.Errorf("нет доступных диалогов")
		}

		scanner := bufio.NewScanner(os.Stdin)

		selected, err := selectChat(chats, scanner)
		if err != nil {
			if errors.Is(err, errAborted) {
				log.Println("Выход без экспорта")
				return nil
			}
			return err
		}
		fmt.Printf("\n✅ Выбран: %s\n", selected.Title)

		limit, err := askMessageLimit(scanner)
		if err != nil {
			if errors.Is(err, errAborted) {
				log.Println("Выход без экспорта")
				return nil
			}
			return err
		}
		fmt.Println()

		outDir := filepath.Join("exports", sanitizeName(selected.Title))
		if err := os.MkdirAll(filepath.Join(outDir, "media"), 0o755); err != nil {
			return fmt.Errorf("создание директории экспорта: %w", err)
		}

		return ExportChat(ctx, api, selected, outDir, limit)
	}); err != nil && appCtx.Err() == nil {
		log.Fatalf("❌ %v", err)
	}
}

// selectChat показывает список диалогов постранично (по chatsPerPage штук)
// и возвращает выбранный пользователем. Номера сквозные по всему списку.
func selectChat(chats []ChatItem, scanner *bufio.Scanner) (ChatItem, error) {
	totalPages := (len(chats) + chatsPerPage - 1) / chatsPerPage
	sep := strings.Repeat("═", 68)
	page := 0

	for {
		start := page * chatsPerPage
		end := min(start+chatsPerPage, len(chats))

		fmt.Printf("\n%s\n", sep)
		fmt.Printf("  Доступные диалоги: %d–%d из %d (страница %d/%d)\n",
			start+1, end, len(chats), page+1, totalPages)
		fmt.Printf("%s\n", sep)
		for i := start; i < end; i++ {
			var typeLabel string
			switch chats[i].Type {
			case "channel":
				typeLabel = "[канал/супергруппа]"
			case "chat":
				typeLabel = "[группа]"
			case "user":
				typeLabel = "[личный чат]"
			}
			fmt.Printf("  %4d. %-48s %s\n", i+1, chats[i].Title, typeLabel)
		}
		fmt.Printf("%s\n", sep)

		hints := []string{}
		if page+1 < totalPages {
			hints = append(hints, "n — следующая страница")
		}
		if page > 0 {
			hints = append(hints, "p — предыдущая")
		}
		hints = append(hints, "q — выход")
		fmt.Printf("  %s\n", strings.Join(hints, ", "))
		fmt.Print("\nВведите номер чата для экспорта: ")

		if !scanner.Scan() {
			return ChatItem{}, errAborted
		}
		input := strings.TrimSpace(scanner.Text())

		switch strings.ToLower(input) {
		case "n", "+", "":
			if page+1 < totalPages {
				page++
			} else {
				fmt.Println("  ⚠️  Это последняя страница")
			}
			continue
		case "p", "-":
			if page > 0 {
				page--
			} else {
				fmt.Println("  ⚠️  Это первая страница")
			}
			continue
		case "q":
			return ChatItem{}, errAborted
		}

		num, err := strconv.Atoi(input)
		if err != nil || num < 1 || num > len(chats) {
			fmt.Printf("  ⚠️  Неверный номер: %q (нужно 1–%d)\n", input, len(chats))
			continue
		}
		return chats[num-1], nil
	}
}

// askMessageLimit спрашивает, сколько последних сообщений экспортировать.
// 0 означает всю историю.
func askMessageLimit(scanner *bufio.Scanner) (int, error) {
	for {
		fmt.Print("Сколько последних сообщений экспортировать (0 — вся история): ")
		if !scanner.Scan() {
			return 0, errAborted
		}
		input := strings.TrimSpace(scanner.Text())
		if strings.EqualFold(input, "q") {
			return 0, errAborted
		}
		n, err := strconv.Atoi(input)
		if err != nil || n < 0 {
			fmt.Printf("  ⚠️  Нужно неотрицательное число, а не %q\n", input)
			continue
		}
		return n, nil
	}
}

func authByQR(ctx context.Context, client *telegram.Client, dispatcher tg.UpdateDispatcher, appID int, appHash string) error {
	loggedIn := qrlogin.OnLoginToken(dispatcher)
	login := qrlogin.NewQR(client.API(), appID, appHash, qrlogin.Options{
		Migrate: func(ctx context.Context, dcID int) error {
			return client.MigrateTo(ctx, dcID)
		},
	})

	_, err := login.Auth(ctx, loggedIn, func(_ context.Context, token qrlogin.Token) error {
		printQR(token.URL())
		fmt.Println("\n⏳ Ожидаю сканирования... (QR обновится автоматически)")
		return nil
	})
	if err != nil {
		if tgerr.Is(err, "SESSION_PASSWORD_NEEDED") {
			log.Println("🔐 Требуется пароль 2FA")
			return handle2FA(ctx, client)
		}
		return err
	}
	return nil
}

func handle2FA(ctx context.Context, client *telegram.Client) error {
	fmt.Print("Введите пароль 2FA: ")
	pwd, err := readPassword()
	if err != nil {
		return err
	}
	fmt.Println()
	if pwd == "" {
		return fmt.Errorf("пароль не введён")
	}
	_, err = client.Auth().Password(ctx, pwd)
	return err
}

func readPassword() (string, error) {
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	if err != nil {
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Scan()
		return strings.TrimSpace(scanner.Text()), nil
	}
	return strings.TrimSpace(string(b)), nil
}

func printQR(data string) {
	code, err := qr.Encode(data, qr.L)
	if err != nil {
		fmt.Printf("❌ QR: %v\n🔗 %s\n", err, data)
		return
	}
	size := code.Size
	border := strings.Repeat("█", size+4)
	fmt.Println("  " + border)
	for y := 0; y < size; y += 2 {
		fmt.Print("  ██")
		for x := 0; x < size; x++ {
			top := code.Black(x, y)
			bottom := y+1 < size && code.Black(x, y+1)
			switch {
			case !top && !bottom:
				fmt.Print("█")
			case !top && bottom:
				fmt.Print("▀")
			case top && !bottom:
				fmt.Print("▄")
			default:
				fmt.Print(" ")
			}
		}
		fmt.Println("██")
	}
	fmt.Println("  " + border)
}

// listChats возвращает все диалоги с пагинацией
func listChats(ctx context.Context, api *tg.Client) ([]ChatItem, error) {
	var result []ChatItem
	seen := make(map[string]bool)
	offsetDate := 0
	offsetID := 0
	var offsetPeer tg.InputPeerClass = &tg.InputPeerEmpty{}

	for {
		resp, err := api.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{
			OffsetDate: offsetDate,
			OffsetID:   offsetID,
			OffsetPeer: offsetPeer,
			Limit:      100,
		})
		if err != nil {
			return nil, fmt.Errorf("getDialogs: %w", err)
		}

		var dialogs []tg.DialogClass
		var chats []tg.ChatClass
		var users []tg.UserClass
		var messages []tg.MessageClass

		switch d := resp.(type) {
		case *tg.MessagesDialogs:
			dialogs, chats, users, messages = d.Dialogs, d.Chats, d.Users, d.Messages
		case *tg.MessagesDialogsSlice:
			dialogs, chats, users, messages = d.Dialogs, d.Chats, d.Users, d.Messages
		default:
			return result, nil
		}

		chatMap := buildChatMap(chats)
		userMap := buildUserMap(users)

		// даты верхних сообщений для пагинации
		msgDates := make(map[int]int)
		for _, m := range messages {
			switch msg := m.(type) {
			case *tg.Message:
				msgDates[msg.ID] = msg.Date
			case *tg.MessageService:
				msgDates[msg.ID] = msg.Date
			}
		}

		for _, d := range dialogs {
			dlg, ok := d.(*tg.Dialog)
			if !ok {
				continue
			}
			item, ok := dialogToItem(dlg, chatMap, userMap)
			if !ok {
				continue
			}
			key := item.Type + ":" + item.Title
			if seen[key] {
				continue
			}
			seen[key] = true
			result = append(result, item)
		}

		if len(dialogs) < 100 {
			break
		}

		// смещение для следующей страницы
		lastDlg, ok := dialogs[len(dialogs)-1].(*tg.Dialog)
		if !ok {
			break
		}
		date, ok := msgDates[lastDlg.TopMessage]
		if !ok || date == 0 {
			break
		}
		offsetDate = date
		offsetID = lastDlg.TopMessage
		offsetPeer = peerToInputPeer(lastDlg.Peer, chatMap, userMap)
		if _, isEmpty := offsetPeer.(*tg.InputPeerEmpty); isEmpty {
			break
		}
	}

	return result, nil
}

func buildChatMap(chats []tg.ChatClass) map[int64]tg.ChatClass {
	m := make(map[int64]tg.ChatClass, len(chats))
	for _, c := range chats {
		switch cc := c.(type) {
		case *tg.Chat:
			m[cc.ID] = cc
		case *tg.Channel:
			m[cc.ID] = cc
		}
	}
	return m
}

func buildUserMap(users []tg.UserClass) map[int64]*tg.User {
	m := make(map[int64]*tg.User, len(users))
	for _, u := range users {
		if usr, ok := u.(*tg.User); ok {
			m[usr.ID] = usr
		}
	}
	return m
}

func dialogToItem(dlg *tg.Dialog, chatMap map[int64]tg.ChatClass, userMap map[int64]*tg.User) (ChatItem, bool) {
	switch peer := dlg.Peer.(type) {
	case *tg.PeerChannel:
		c, ok := chatMap[peer.ChannelID]
		if !ok {
			return ChatItem{}, false
		}
		ch, ok := c.(*tg.Channel)
		if !ok || ch.Left {
			return ChatItem{}, false
		}
		return ChatItem{
			Title:     ch.Title,
			InputPeer: &tg.InputPeerChannel{ChannelID: ch.ID, AccessHash: ch.AccessHash},
			Type:      "channel",
		}, true

	case *tg.PeerChat:
		c, ok := chatMap[peer.ChatID]
		if !ok {
			return ChatItem{}, false
		}
		ch, ok := c.(*tg.Chat)
		if !ok || ch.Deactivated || ch.Left {
			return ChatItem{}, false
		}
		return ChatItem{
			Title:     ch.Title,
			InputPeer: &tg.InputPeerChat{ChatID: ch.ID},
			Type:      "chat",
		}, true

	case *tg.PeerUser:
		u, ok := userMap[peer.UserID]
		if !ok || u.Bot || u.Self || u.Deleted {
			return ChatItem{}, false
		}
		name := strings.TrimSpace(u.FirstName + " " + u.LastName)
		if name == "" {
			name = fmt.Sprintf("User_%d", u.ID)
		}
		if username, ok2 := u.GetUsername(); ok2 && username != "" {
			name += " (@" + username + ")"
		}
		return ChatItem{
			Title:     name,
			InputPeer: &tg.InputPeerUser{UserID: u.ID, AccessHash: u.AccessHash},
			Type:      "user",
		}, true
	}
	return ChatItem{}, false
}

func peerToInputPeer(peer tg.PeerClass, chatMap map[int64]tg.ChatClass, userMap map[int64]*tg.User) tg.InputPeerClass {
	switch p := peer.(type) {
	case *tg.PeerChannel:
		if c, ok := chatMap[p.ChannelID]; ok {
			if ch, ok := c.(*tg.Channel); ok {
				return &tg.InputPeerChannel{ChannelID: ch.ID, AccessHash: ch.AccessHash}
			}
		}
	case *tg.PeerChat:
		return &tg.InputPeerChat{ChatID: p.ChatID}
	case *tg.PeerUser:
		if u, ok := userMap[p.UserID]; ok {
			return &tg.InputPeerUser{UserID: u.ID, AccessHash: u.AccessHash}
		}
	}
	return &tg.InputPeerEmpty{}
}

func sanitizeName(name string) string {
	var b strings.Builder
	count := 0
	for _, r := range name { // итерируем по рунам, чтобы не обрезать UTF-8 посередине
		if count >= 64 {
			break
		}
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|', '\n', '\r', '\t', ' ':
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
		count++
	}
	result := b.String()
	if result == "" {
		result = "unnamed"
	}
	return result
}
