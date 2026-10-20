package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

type mediaFixture struct {
	name      string
	media     tg.MessageMediaClass
	filename  string // Формат с ID сообщения.
	mediaType string
	sticker   bool
	audio     bool
}

func mediaFixtures() []mediaFixture {
	document := func(mimeType string, attrs ...tg.DocumentAttributeClass) tg.MessageMediaClass {
		return &tg.MessageMediaDocument{Document: &tg.Document{MimeType: mimeType, Attributes: attrs}}
	}
	return []mediaFixture{
		{
			name: "photo",
			media: &tg.MessageMediaPhoto{Photo: &tg.Photo{
				Sizes: []tg.PhotoSizeClass{&tg.PhotoSize{Type: "x"}},
			}},
			filename: "photo_%d_x.jpg", mediaType: "photo",
		},
		{
			name:     "static sticker",
			media:    document("image/webp", &tg.DocumentAttributeSticker{Stickerset: &tg.InputStickerSetEmpty{}}),
			filename: "doc_%d.webp", mediaType: "sticker", sticker: true,
		},
		{
			name:     "animated sticker with animation attribute first",
			media:    document("application/x-tgsticker", &tg.DocumentAttributeAnimated{}, &tg.DocumentAttributeSticker{Stickerset: &tg.InputStickerSetEmpty{}}),
			filename: "doc_%d.tgs", mediaType: "animation", sticker: true,
		},
		{
			name:     "video sticker with video attribute first",
			media:    document("video/webm", &tg.DocumentAttributeVideo{}, &tg.DocumentAttributeSticker{Stickerset: &tg.InputStickerSetEmpty{}}),
			filename: "doc_%d.webm", mediaType: "video", sticker: true,
		},
		{
			name:     "video sticker with sticker attribute first",
			media:    document("video/webm", &tg.DocumentAttributeSticker{Stickerset: &tg.InputStickerSetEmpty{}}, &tg.DocumentAttributeVideo{}),
			filename: "doc_%d.webm", mediaType: "sticker", sticker: true,
		},
		{
			name:     "audio",
			media:    document("audio/mpeg", &tg.DocumentAttributeAudio{}),
			filename: "doc_%d.mp3", mediaType: "audio", audio: true,
		},
		{
			name:     "voice",
			media:    document("audio/ogg", &tg.DocumentAttributeAudio{Voice: true}),
			filename: "doc_%d.ogg", mediaType: "voice", audio: true,
		},
		{
			name:     "audio without audio MIME",
			media:    document("application/x-tg-export-unknown-audio", &tg.DocumentAttributeAudio{}),
			filename: "doc_%d.ogg", mediaType: "audio", audio: true,
		},
		{
			name:     "audio without attributes",
			media:    document("audio/mpeg"),
			filename: "doc_%d.mp3", mediaType: "document", audio: true,
		},
		{
			name:     "audio MIME with parameters",
			media:    document("audio/ogg; codecs=opus", &tg.DocumentAttributeFilename{FileName: "voice.ogg"}),
			filename: "doc_%d_voice.ogg", mediaType: "document", audio: true,
		},
		{
			name:     "video note",
			media:    document("video/mp4", &tg.DocumentAttributeVideo{RoundMessage: true}),
			filename: "doc_%d.mp4", mediaType: "video_note", audio: true,
		},
		{
			name:     "ordinary video",
			media:    document("video/mp4", &tg.DocumentAttributeVideo{}),
			filename: "doc_%d.mp4", mediaType: "video",
		},
		{
			name:     "ordinary animation",
			media:    document("image/gif", &tg.DocumentAttributeAnimated{}),
			filename: "doc_%d.gif", mediaType: "animation",
		},
		{
			name:     "ordinary webp document",
			media:    document("image/webp"),
			filename: "doc_%d.webp", mediaType: "document",
		},
		{
			name:     "named document",
			media:    document("application/pdf", &tg.DocumentAttributeFilename{FileName: "report final.pdf"}),
			filename: "doc_%d_report_final.pdf", mediaType: "document",
		},
		{name: "empty photo", media: &tg.MessageMediaPhoto{}, mediaType: "photo"},
		{name: "empty document", media: &tg.MessageMediaDocument{}, mediaType: "document"},
		{name: "geo", media: &tg.MessageMediaGeo{}, mediaType: "geo"},
		{name: "contact", media: &tg.MessageMediaContact{}, mediaType: "contact"},
		{name: "poll", media: &tg.MessageMediaPoll{}, mediaType: "poll"},
		{name: "webpage", media: &tg.MessageMediaWebPage{}, mediaType: "webpage"},
		{name: "unsupported", media: &tg.MessageMediaUnsupported{}, mediaType: "*tg.MessageMediaUnsupported"},
	}
}

func (f mediaFixture) relPath(msgID int) string {
	if f.filename == "" {
		return ""
	}
	return filepath.Join("media", fmt.Sprintf(f.filename, msgID))
}

func (f mediaFixture) skipped(opts ExportOptions) bool {
	return opts.NoMedia || (opts.NoStickers && f.sticker) || (opts.NoAudio && f.audio)
}

func exportOptionCombinations() []ExportOptions {
	var result []ExportOptions
	for bits := 0; bits < 8; bits++ {
		result = append(result, ExportOptions{
			NoStickers: bits&1 != 0,
			NoAudio:    bits&2 != 0,
			NoMedia:    bits&4 != 0,
		})
	}
	return result
}

func TestSkipDocumentOptions(t *testing.T) {
	for _, fixture := range mediaFixtures() {
		media, ok := fixture.media.(*tg.MessageMediaDocument)
		if !ok {
			continue
		}
		doc, ok := media.Document.(*tg.Document)
		if !ok {
			continue
		}
		t.Run(fixture.name, func(t *testing.T) {
			for _, opts := range exportOptionCombinations() {
				if got, want := opts.skipDocument(doc), fixture.skipped(opts); got != want {
					t.Errorf("skipDocument with %+v = %v, want %v", opts, got, want)
				}
			}
		})
	}
}

func TestDownloadMediaOptions(t *testing.T) {
	const msgID = 6204
	for _, fixture := range mediaFixtures() {
		t.Run(fixture.name, func(t *testing.T) {
			for _, opts := range exportOptionCombinations() {
				t.Run(fmt.Sprintf("%+v", opts), func(t *testing.T) {
					outDir := t.TempDir()
					wantPath := fixture.relPath(msgID)
					if wantPath != "" && !fixture.skipped(opts) {
						writeTestMedia(t, outDir, wantPath)
					}
					// Без клиента и downloader запрещённое вложение обязано
					// вернуть метаданные, не пытаясь обратиться к Telegram.
					gotPath, gotType, err := downloadMedia(context.Background(), nil, nil, fixture.media, outDir, msgID, nil, opts)
					if err != nil {
						t.Fatal(err)
					}
					if gotPath != wantPath || gotType != fixture.mediaType {
						t.Fatalf("got (%q, %q), want (%q, %q)", gotPath, gotType, wantPath, fixture.mediaType)
					}
					if fixture.skipped(opts) {
						entries, err := os.ReadDir(outDir)
						if err != nil {
							t.Fatal(err)
						}
						if len(entries) != 0 {
							t.Fatalf("skipped media created files: %v", entries)
						}
					}
				})
			}
		})
	}
}

func writeTestMedia(t *testing.T, outDir, relPath string) {
	t.Helper()
	path := filepath.Join(outDir, relPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("existing media"), 0o600); err != nil {
		t.Fatal(err)
	}
}

type historyInvoker struct {
	response *tg.MessagesMessages
	requests int
}

func (h *historyInvoker) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	if _, ok := input.(*tg.MessagesGetHistoryRequest); !ok {
		return fmt.Errorf("unexpected Telegram request: %T", input)
	}
	h.requests++
	buffer := &bin.Buffer{}
	if err := h.response.Encode(buffer); err != nil {
		return err
	}
	return output.Decode(buffer)
}

func TestExportChatJSONLUnchanged(t *testing.T) {
	// Отдельно проверяем полный путь экспорта, включая сообщения без текста.
	var fixtures []mediaFixture
	for _, fixture := range mediaFixtures() {
		if fixture.filename != "" {
			fixtures = append(fixtures, fixture)
		}
	}
	messages := []tg.MessageClass{&tg.Message{
		ID: 6300, Date: 1700000000, Message: "text only", PeerID: &tg.PeerChat{ChatID: 1},
	}}
	for i, fixture := range fixtures {
		msg := &tg.Message{ID: 6204 + i, Date: 1700000000, PeerID: &tg.PeerChat{ChatID: 1}}
		if i%2 == 0 {
			msg.Message = "caption"
		}
		msg.SetMedia(fixture.media)
		messages = append(messages, msg)
	}

	var baseline string
	for _, opts := range exportOptionCombinations() {
		t.Run(fmt.Sprintf("%+v", opts), func(t *testing.T) {
			outDir := t.TempDir()
			for i, fixture := range fixtures {
				if !fixture.skipped(opts) {
					writeTestMedia(t, outDir, fixture.relPath(6204+i))
				}
			}
			invoker := &historyInvoker{response: &tg.MessagesMessages{Messages: messages}}
			chat := ChatItem{Title: "test", InputPeer: &tg.InputPeerSelf{}}
			if err := ExportChat(context.Background(), tg.NewClient(invoker), chat, outDir, 0, opts); err != nil {
				t.Fatal(err)
			}
			if invoker.requests != 1 {
				t.Fatalf("history requests = %d, want 1", invoker.requests)
			}
			data, err := os.ReadFile(filepath.Join(outDir, "messages.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			if baseline == "" {
				baseline = string(data)
			}
			if string(data) != baseline {
				t.Fatalf("JSONL changed with options %+v:\n%s\nwant:\n%s", opts, data, baseline)
			}
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			if len(lines) != len(messages) {
				t.Fatalf("JSONL has %d messages, want %d", len(lines), len(messages))
			}
			for i, fixture := range fixtures {
				var msg MessageJSON
				if err := json.Unmarshal([]byte(lines[i+1]), &msg); err != nil {
					t.Fatal(err)
				}
				wantPath := fixture.relPath(6204 + i)
				if msg.MediaFile != wantPath || msg.MediaType != fixture.mediaType {
					t.Fatalf("%s: media = (%q, %q), want (%q, %q)", fixture.name, msg.MediaFile, msg.MediaType, wantPath, fixture.mediaType)
				}
				_, err := os.Stat(filepath.Join(outDir, wantPath))
				if fixture.skipped(opts) {
					if !os.IsNotExist(err) {
						t.Fatalf("%s: skipped file exists or stat failed: %v", fixture.name, err)
					}
				} else if err != nil {
					t.Fatalf("%s: existing file disappeared: %v", fixture.name, err)
				}
			}
		})
	}
}

func TestSkippedMediaKeepsExistingFiles(t *testing.T) {
	for _, fixture := range mediaFixtures() {
		if fixture.filename == "" {
			continue
		}
		for _, opts := range exportOptionCombinations() {
			if !fixture.skipped(opts) {
				continue
			}
			t.Run(fmt.Sprintf("%s/%+v", fixture.name, opts), func(t *testing.T) {
				outDir := t.TempDir()
				relPath := fixture.relPath(6204)
				writeTestMedia(t, outDir, relPath)
				path := filepath.Join(outDir, relPath)
				before, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err := downloadMedia(context.Background(), nil, nil, fixture.media, outDir, 6204, nil, opts); err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				after, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if string(data) != "existing media" || !before.ModTime().Equal(after.ModTime()) {
					t.Fatalf("existing file changed: contents=%q, mtime %v -> %v", data, before.ModTime(), after.ModTime())
				}
			})
		}
	}
}
