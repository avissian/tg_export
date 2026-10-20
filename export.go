package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// batchSize — максимальный размер одного запроса истории (ограничение API)
const batchSize = 100

// MessageJSON — структура одного сообщения в JSONL-файле
type MessageJSON struct {
	ID           int          `json:"id"`
	Date         time.Time    `json:"date"`
	FromID       int64        `json:"from_id,omitempty"`
	FromName     string       `json:"from_name,omitempty"`
	Text         string       `json:"text,omitempty"`
	ReplyToMsgID int          `json:"reply_to,omitempty"`
	ForwardFrom  *ForwardInfo `json:"forwarded_from,omitempty"`
	MediaFile    string       `json:"media_file,omitempty"`
	MediaType    string       `json:"media_type,omitempty"`
	Edited       bool         `json:"edited,omitempty"`
}

// ForwardInfo описывает источник пересланного сообщения
type ForwardInfo struct {
	FromID   int64     `json:"from_id,omitempty"`
	FromName string    `json:"from_name,omitempty"`
	Date     time.Time `json:"date"`
}

// ExportOptions управляет скачиванием файлов, не меняя содержимое JSONL.
// Нулевые настройки разрешают скачивание всех вложений.
type ExportOptions struct {
	NoStickers bool
	NoAudio    bool // Аудиофайлы, голосовые сообщения и видеокружочки.
	NoMedia    bool
}

func (opts ExportOptions) skipDocument(doc *tg.Document) bool {
	if opts.NoMedia || (opts.NoAudio && strings.HasPrefix(doc.MimeType, "audio/")) {
		return true
	}
	// Проверяем все атрибуты независимо от classifyDocument: у видеостикера
	// атрибут видео может идти раньше атрибута стикера.
	for _, attr := range doc.Attributes {
		switch a := attr.(type) {
		case *tg.DocumentAttributeSticker:
			if opts.NoStickers {
				return true
			}
		case *tg.DocumentAttributeAudio:
			if opts.NoAudio {
				return true
			}
		case *tg.DocumentAttributeVideo:
			if opts.NoAudio && a.RoundMessage {
				return true
			}
		}
	}
	return false
}

// ExportChat экспортирует сообщения чата в директорию outDir.
// limit — сколько последних сообщений выгрузить, 0 означает всю историю.
// Файлы: outDir/messages.jsonl (сообщения от новых к старым),
//
//	outDir/media/ (фото, видео, документы и т.д.)
func ExportChat(ctx context.Context, api *tg.Client, chat ChatItem, outDir string, limit int, opts ExportOptions) error {
	msgFile, err := os.Create(filepath.Join(outDir, "messages.jsonl"))
	if err != nil {
		return fmt.Errorf("создание messages.jsonl: %w", err)
	}
	defer msgFile.Close()

	encoder := json.NewEncoder(msgFile)
	dl := downloader.NewDownloader()

	offsetID := 0
	totalMsgs := 0
	batchNum := 0

	log.Printf("📥 Начинаю экспорт: %s", chat.Title)
	log.Printf("   Директория: %s", outDir)
	if limit > 0 {
		log.Printf("   Лимит: %d последних сообщений", limit)
	} else {
		log.Println("   Лимит: вся история")
	}
	log.Println("   Сообщения сохраняются от новых к старым.")

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		batchLimit := batchSize
		if limit > 0 {
			remaining := limit - totalMsgs
			if remaining <= 0 {
				break
			}
			batchLimit = min(remaining, batchSize)
		}

		resp, err := fetchHistoryWithRetry(ctx, api, chat.InputPeer, offsetID, batchLimit)
		if err != nil {
			return fmt.Errorf("получение истории: %w", err)
		}

		var messages []tg.MessageClass
		var users []tg.UserClass
		var chats []tg.ChatClass

		switch m := resp.(type) {
		case *tg.MessagesMessages:
			messages, users, chats = m.Messages, m.Users, m.Chats
		case *tg.MessagesMessagesSlice:
			messages, users, chats = m.Messages, m.Users, m.Chats
		case *tg.MessagesChannelMessages:
			messages, users, chats = m.Messages, m.Users, m.Chats
		default:
			return fmt.Errorf("неизвестный тип ответа: %T", resp)
		}

		if len(messages) == 0 {
			break
		}

		userMap := buildUserMap(users)
		chatNameMap := make(map[int64]string, len(chats))
		for _, c := range chats {
			switch cc := c.(type) {
			case *tg.Chat:
				chatNameMap[cc.ID] = cc.Title
			case *tg.Channel:
				chatNameMap[cc.ID] = cc.Title
			}
		}

		minID := 0
		for _, rawMsg := range messages {
			id := messageID(rawMsg)
			if id > 0 && (minID == 0 || id < minID) {
				minID = id
			}

			msg, ok := rawMsg.(*tg.Message)
			if !ok {
				continue
			}

			jmsg := buildMessageJSON(msg, userMap, chatNameMap)

			// Медиа
			if media, ok := msg.GetMedia(); ok {
				relPath, mediaType, dlErr := downloadMedia(ctx, api, dl, media, outDir, msg.ID, chat.InputPeer, opts)
				if dlErr != nil {
					log.Printf("⚠️  Медиа для сообщения %d: %v", msg.ID, dlErr)
				}
				jmsg.MediaFile = relPath
				jmsg.MediaType = mediaType
			}

			if err := encoder.Encode(jmsg); err != nil {
				return fmt.Errorf("запись сообщения %d: %w", msg.ID, err)
			}
		}

		totalMsgs += len(messages)
		batchNum++
		if limit > 0 {
			log.Printf("   Пакет %-4d → %d сообщений в пакете, всего: %d из %d (последний ID: %d)",
				batchNum, len(messages), totalMsgs, limit, minID)
		} else {
			log.Printf("   Пакет %-4d → %d сообщений в пакете, всего: %d (последний ID: %d)",
				batchNum, len(messages), totalMsgs, minID)
		}

		// Ответ короче запрошенного — история кончилась
		if len(messages) < batchLimit {
			break
		}
		offsetID = minID

		// небольшая пауза, чтобы не провоцировать FLOOD_WAIT
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(400 * time.Millisecond):
		}
	}

	log.Printf("✅ Экспорт завершён! Всего сообщений: %d", totalMsgs)
	log.Printf("   Файл: %s", filepath.Join(outDir, "messages.jsonl"))
	return nil
}

// fetchHistoryWithRetry выполняет запрос истории с повтором при FLOOD_WAIT
// и при обрыве соединения (engine forcibly closed).
func fetchHistoryWithRetry(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, offsetID, limit int) (tg.MessagesMessagesClass, error) {
	for attempt := 0; attempt < 10; attempt++ {
		resp, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
			Peer:     peer,
			OffsetID: offsetID,
			Limit:    limit,
		})
		if err == nil {
			return resp, nil
		}
		if d, ok := tgerr.AsFloodWait(err); ok {
			wait := d + 5*time.Second
			log.Printf("⏳ FLOOD_WAIT: жду %.0f сек...", wait.Seconds())
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
			continue
		}
		// Обрыв внутреннего соединения gotd — не наш context.Cancel
		if errors.Is(err, context.Canceled) && ctx.Err() == nil {
			log.Printf("⚠️  Соединение сброшено, повтор через 3 сек... (попытка %d)", attempt+1)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(3 * time.Second):
			}
			continue
		}
		return nil, err
	}
	return nil, fmt.Errorf("превышено число попыток получения истории")
}

func buildMessageJSON(msg *tg.Message, userMap map[int64]*tg.User, chatNameMap map[int64]string) MessageJSON {
	jmsg := MessageJSON{
		ID:   msg.ID,
		Date: time.Unix(int64(msg.Date), 0).UTC(),
		Text: msg.Message,
	}

	if _, ok := msg.GetEditDate(); ok {
		jmsg.Edited = true
	}

	// Отправитель
	if fromID, ok := msg.GetFromID(); ok {
		switch p := fromID.(type) {
		case *tg.PeerUser:
			jmsg.FromID = p.UserID
			if u, ok := userMap[p.UserID]; ok {
				jmsg.FromName = formatUserName(u)
			}
		case *tg.PeerChannel:
			jmsg.FromID = p.ChannelID
			jmsg.FromName = chatNameMap[p.ChannelID]
		}
	}

	// Ответ на сообщение
	if replyHdr, ok := msg.GetReplyTo(); ok {
		if rh, ok := replyHdr.(*tg.MessageReplyHeader); ok {
			if replyMsgID, ok := rh.GetReplyToMsgID(); ok {
				jmsg.ReplyToMsgID = replyMsgID
			}
		}
	}

	// Пересланное сообщение
	if fwd, ok := msg.GetFwdFrom(); ok {
		jmsg.ForwardFrom = &ForwardInfo{
			Date: time.Unix(int64(fwd.Date), 0).UTC(),
		}
		if fromID, ok := fwd.GetFromID(); ok {
			switch p := fromID.(type) {
			case *tg.PeerUser:
				jmsg.ForwardFrom.FromID = p.UserID
				if u, ok := userMap[p.UserID]; ok {
					jmsg.ForwardFrom.FromName = formatUserName(u)
				}
			case *tg.PeerChannel:
				jmsg.ForwardFrom.FromID = p.ChannelID
				jmsg.ForwardFrom.FromName = chatNameMap[p.ChannelID]
			}
		}
		if jmsg.ForwardFrom.FromName == "" {
			if name, ok := fwd.GetFromName(); ok {
				jmsg.ForwardFrom.FromName = name
			}
		}
	}

	return jmsg
}

func formatUserName(u *tg.User) string {
	name := strings.TrimSpace(u.FirstName + " " + u.LastName)
	if username, ok := u.GetUsername(); ok && username != "" {
		name += " (@" + username + ")"
	}
	return name
}

// messageID универсально извлекает ID из любого типа MessageClass
func messageID(m tg.MessageClass) int {
	switch mm := m.(type) {
	case *tg.Message:
		return mm.ID
	case *tg.MessageService:
		return mm.ID
	case *tg.MessageEmpty:
		return mm.ID
	}
	return 0
}

// downloadMedia скачивает медиафайл и возвращает относительный путь и тип.
// Если скачивание отключено настройками — возвращает те же путь и тип без загрузки.
// Если файл уже существует — пропускает скачивание.
// При FILE_REFERENCE_EXPIRED перезапрашивает сообщение и повторяет попытку.
func downloadMedia(ctx context.Context, api *tg.Client, dl *downloader.Downloader, media tg.MessageMediaClass, outDir string, msgID int, peer tg.InputPeerClass, opts ExportOptions) (relPath, mediaType string, err error) {
	for attempt := 0; attempt < 2; attempt++ {
		relPath, mediaType, err = doDownloadMedia(ctx, api, dl, media, outDir, msgID, opts)
		if err == nil || !tgerr.Is(err, "FILE_REFERENCE_EXPIRED") || attempt > 0 {
			return
		}
		// FILE_REFERENCE_EXPIRED: перезапрашиваем сообщение за свежим file reference
		log.Printf("🔄 Обновляю file reference для сообщения %d...", msgID)
		freshMedia, refreshErr := refreshMedia(ctx, api, peer, msgID)
		if refreshErr != nil {
			return "", mediaType, fmt.Errorf("обновление file reference: %w", refreshErr)
		}
		media = freshMedia
	}
	return
}

// refreshMedia перезапрашивает сообщение для получения свежего file reference
func refreshMedia(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, msgID int) (tg.MessageMediaClass, error) {
	inputMsg := []tg.InputMessageClass{&tg.InputMessageID{ID: msgID}}
	var messages []tg.MessageClass

	switch p := peer.(type) {
	case *tg.InputPeerChannel:
		resp, err := api.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
			Channel: &tg.InputChannel{ChannelID: p.ChannelID, AccessHash: p.AccessHash},
			ID:      inputMsg,
		})
		if err != nil {
			return nil, fmt.Errorf("channels.getMessages: %w", err)
		}
		if r, ok := resp.(*tg.MessagesChannelMessages); ok {
			messages = r.Messages
		}
	default:
		resp, err := api.MessagesGetMessages(ctx, inputMsg)
		if err != nil {
			return nil, fmt.Errorf("messages.getMessages: %w", err)
		}
		switch r := resp.(type) {
		case *tg.MessagesMessages:
			messages = r.Messages
		case *tg.MessagesMessagesSlice:
			messages = r.Messages
		}
	}

	for _, m := range messages {
		if msg, ok := m.(*tg.Message); ok && msg.ID == msgID {
			if media, ok := msg.GetMedia(); ok {
				return media, nil
			}
		}
	}
	return nil, fmt.Errorf("медиа для сообщения %d не найдено при обновлении", msgID)
}

// doDownloadMedia — одна попытка скачать медиа (без retry по file reference)
func doDownloadMedia(ctx context.Context, api *tg.Client, dl *downloader.Downloader, media tg.MessageMediaClass, outDir string, msgID int, opts ExportOptions) (relPath, mediaType string, err error) {
	mediaDir := filepath.Join(outDir, "media")

	switch m := media.(type) {
	case *tg.MessageMediaPhoto:
		photo, ok := m.Photo.(*tg.Photo)
		if !ok {
			return "", "photo", nil
		}
		thumbSize := bestPhotoThumb(photo.Sizes)
		filename := fmt.Sprintf("photo_%d_%s.jpg", msgID, thumbSize)
		if opts.NoMedia {
			return filepath.Join("media", filename), "photo", nil
		}
		filePath := filepath.Join(mediaDir, filename)

		if _, statErr := os.Stat(filePath); os.IsNotExist(statErr) {
			loc := &tg.InputPhotoFileLocation{
				ID:            photo.ID,
				AccessHash:    photo.AccessHash,
				FileReference: photo.FileReference,
				ThumbSize:     thumbSize,
			}
			if err := downloadWithRetry(ctx, api, dl, loc, filePath); err != nil {
				return "", "photo", err
			}
		}
		return filepath.Join("media", filename), "photo", nil

	case *tg.MessageMediaDocument:
		doc, ok := m.Document.(*tg.Document)
		if !ok {
			return "", "document", nil
		}
		mt := classifyDocument(doc)
		filename := extractDocFilename(doc, msgID)
		if opts.skipDocument(doc) {
			return filepath.Join("media", filename), mt, nil
		}
		filePath := filepath.Join(mediaDir, filename)

		if _, statErr := os.Stat(filePath); os.IsNotExist(statErr) {
			loc := &tg.InputDocumentFileLocation{
				ID:            doc.ID,
				AccessHash:    doc.AccessHash,
				FileReference: doc.FileReference,
				ThumbSize:     "",
			}
			if err := downloadWithRetry(ctx, api, dl, loc, filePath); err != nil {
				return "", mt, err
			}
		}
		return filepath.Join("media", filename), mt, nil

	case *tg.MessageMediaGeo:
		return "", "geo", nil
	case *tg.MessageMediaContact:
		return "", "contact", nil
	case *tg.MessageMediaPoll:
		return "", "poll", nil
	case *tg.MessageMediaWebPage:
		return "", "webpage", nil
	default:
		return "", fmt.Sprintf("%T", media), nil
	}
}

// downloadWithRetry скачивает файл с повтором при FLOOD_WAIT и обрыве соединения
func downloadWithRetry(ctx context.Context, api *tg.Client, dl *downloader.Downloader, loc tg.InputFileLocationClass, filePath string) error {
	for attempt := 0; attempt < 5; attempt++ {
		err := downloadToFile(ctx, api, dl, loc, filePath)
		if err == nil {
			return nil
		}
		if d, ok := tgerr.AsFloodWait(err); ok {
			wait := d + 3*time.Second
			log.Printf("⏳ Flood wait при скачивании: %.0f сек...", wait.Seconds())
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			continue
		}
		// Обрыв внутреннего соединения gotd — не наш context.Cancel
		if errors.Is(err, context.Canceled) && ctx.Err() == nil {
			log.Printf("⚠️  Соединение сброшено при скачивании, повтор через 3 сек... (попытка %d)", attempt+1)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(3 * time.Second):
			}
			continue
		}
		return err
	}
	return fmt.Errorf("не удалось скачать файл после 5 попыток")
}

func downloadToFile(ctx context.Context, api *tg.Client, dl *downloader.Downloader, loc tg.InputFileLocationClass, filePath string) (retErr error) {
	f, err := os.Create(filePath)
	if err != nil {
		return fmt.Errorf("создание файла: %w", err)
	}
	defer func() {
		f.Close()
		if retErr != nil {
			os.Remove(filePath)
		}
	}()
	_, retErr = dl.Download(api, loc).Stream(ctx, f)
	return retErr
}

// bestPhotoThumb выбирает лучший (наибольший) размер превью фото
func bestPhotoThumb(sizes []tg.PhotoSizeClass) string {
	priority := []string{"y", "x", "w", "b", "s"}
	for _, p := range priority {
		for _, sz := range sizes {
			if ps, ok := sz.(*tg.PhotoSize); ok && ps.Type == p {
				return p
			}
		}
	}
	for _, sz := range sizes {
		if ps, ok := sz.(*tg.PhotoSizeProgressive); ok {
			return ps.Type
		}
	}
	return "y"
}

// extractDocFilename формирует имя файла для документа
func extractDocFilename(doc *tg.Document, msgID int) string {
	for _, attr := range doc.Attributes {
		if fn, ok := attr.(*tg.DocumentAttributeFilename); ok {
			safe := sanitizeName(fn.FileName)
			return fmt.Sprintf("doc_%d_%s", msgID, safe)
		}
	}
	ext := extensionFromMIME(doc.MimeType, doc.Attributes)
	return fmt.Sprintf("doc_%d%s", msgID, ext)
}

// extensionFromMIME возвращает расширение файла по MIME-типу и атрибутам
func extensionFromMIME(mimeType string, attrs []tg.DocumentAttributeClass) string {
	knownExts := map[string]string{
		"video/mp4":               ".mp4",
		"video/webm":              ".webm",
		"video/quicktime":         ".mov",
		"audio/ogg":               ".ogg",
		"audio/mpeg":              ".mp3",
		"audio/mp4":               ".m4a",
		"audio/aac":               ".aac",
		"image/jpeg":              ".jpg",
		"image/png":               ".png",
		"image/gif":               ".gif",
		"image/webp":              ".webp",
		"application/pdf":         ".pdf",
		"application/zip":         ".zip",
		"application/x-tgsticker": ".tgs",
		"text/plain":              ".txt",
	}
	if ext, ok := knownExts[mimeType]; ok {
		return ext
	}
	// Пробуем стандартную библиотеку
	if exts, err := mime.ExtensionsByType(mimeType); err == nil && len(exts) > 0 {
		return exts[0]
	}
	// Определяем по атрибутам
	for _, attr := range attrs {
		switch attr.(type) {
		case *tg.DocumentAttributeVideo:
			return ".mp4"
		case *tg.DocumentAttributeAudio:
			return ".ogg"
		case *tg.DocumentAttributeAnimated:
			return ".gif"
		case *tg.DocumentAttributeSticker:
			return ".webp"
		}
	}
	return ".bin"
}

// classifyDocument определяет тип документа
func classifyDocument(doc *tg.Document) string {
	for _, attr := range doc.Attributes {
		switch a := attr.(type) {
		case *tg.DocumentAttributeVideo:
			if a.RoundMessage {
				return "video_note"
			}
			return "video"
		case *tg.DocumentAttributeAudio:
			if a.Voice {
				return "voice"
			}
			return "audio"
		case *tg.DocumentAttributeSticker:
			return "sticker"
		case *tg.DocumentAttributeAnimated:
			return "animation"
		}
	}
	return "document"
}
