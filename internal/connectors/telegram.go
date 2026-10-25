package connectors

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"path"
	"strings"
	"time"
)

// TelegramOrigin is the exact Telegram API origin. The operator must grant it
// for the connector before any Telegram call is attempted.
const TelegramOrigin = "https://api.telegram.org"

// TelegramMessageLimit is the reply size accepted by sendMessage.
const TelegramMessageLimit = 4096

// Telegram implements the Telegram long polling transport. Polling is explicit:
// no webhook is ever installed or deleted by this transport.
type Telegram struct {
	cfg   Config
	token string
	http *providerHTTP
}

// NewTelegram builds a Telegram transport from a resolved bot token.
func NewTelegram(ctx context.Context, cfg Config) (*Telegram, error) {
	if cfg.Token == "" {
		return nil, errors.New("telegram connector requires a bot token")
	}
	if cfg.Egress.Client == nil {
		return nil, errors.New("telegram connector requires approved egress")
	}
	return &Telegram{cfg: cfg, token: cfg.Token, http: &providerHTTP{egress: cfg.Egress}}, nil
}

// Kind reports the provider.
func (t *Telegram) Kind() Kind { return KindTelegram }

// Close releases provider resources.
func (t *Telegram) Close() error { return nil }

// telegramUpdate is one long polling update.
type telegramUpdate struct {
	UpdateID int64            `json:"update_id"`
	Message  *telegramMessage `json:"message"`
}

// telegramMessage is one inbound message.
type telegramMessage struct {
	MessageID int64           `json:"message_id"`
	From      *telegramUser   `json:"from"`
	Chat      *telegramChat   `json:"chat"`
	Date      int64           `json:"date"`
	Text      string          `json:"text"`
	Caption   string          `json:"caption"`
	Document  *telegramFile   `json:"document"`
	Photo     []telegramFile  `json:"photo"`
}

// telegramUser is the author of a message.
type telegramUser struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
}

// telegramChat is the conversation a message belongs to.
type telegramChat struct {
	ID    int64  `json:"id"`
	Type  string `json:"type"`
	Title string `json:"title"`
}

// telegramFile is an attached file.
type telegramFile struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileName     string `json:"file_name"`
	MimeType     string `json:"mime_type"`
	FileSize     int64  `json:"file_size"`
}

// apiResponse is the common Telegram response envelope.
type apiResponse struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description"`
	Result      json.RawMessage `json:"result"`
}

// Test verifies the bot token and reports an existing webhook instead of
// removing it.
func (t *Telegram) Test(ctx context.Context) error {
	if err := t.checkWebhook(ctx); err != nil {
		return err
	}
	_, err := t.call(ctx, "getMe", nil)
	if err != nil {
		return Definite("telegram credential check failed: %s", Scrub(err, t.token))
	}
	return nil
}

// checkWebhook reports an already installed webhook. The transport never
// deletes a webhook the operator configured elsewhere.
func (t *Telegram) checkWebhook(ctx context.Context) error {
	response, err := t.call(ctx, "getWebhookInfo", nil)
	if err != nil {
		return Uncertain("telegram webhook check failed: %s", Scrub(err, t.token))
	}
	var info struct {
		URL string `json:"url"`
	}
	if err = json.Unmarshal(response, &info); err != nil {
		return Definite("telegram webhook check returned an unreadable result")
	}
	if strings.TrimSpace(info.URL) != "" {
		return WebhookConflict("telegram already has a webhook configured; remove it outside Meshra before using polling")
	}
	return nil
}

// Receive polls for updates until the context is cancelled. The offset advances
// only after an update is durably recorded.
func (t *Telegram) Receive(ctx context.Context, sink Sink) error {
	if err := t.checkWebhook(ctx); err != nil {
		return err
	}
	offset := int64(0)
	if t.cfg.Cursor != nil {
		cursor, err := t.cfg.Cursor(ctx)
		if err != nil { return errors.New("telegram durable cursor could not be read") }
		if cursor != "" {
			parsed, perr := strconv.ParseInt(cursor, 10, 64)
			if perr != nil || parsed < 0 { return errors.New("telegram durable cursor is invalid") }
			offset = parsed
		}
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		updates, err := t.updates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// A polling failure is not a delivery failure: the offset is not
			// advanced and the same updates are requested again.
			if waitContext(ctx, 2*time.Second) != nil { return ctx.Err() }
			continue
		}
		for _, update := range updates {
			if update.UpdateID < offset { continue }
			event, ok := t.translate(update)
			if ok && !sink(event) { return errors.New("telegram event is awaiting durable acceptance") }
			next := update.UpdateID + 1
			if t.cfg.SaveCursor != nil {
				if err := t.cfg.SaveCursor(ctx, strconv.FormatInt(next, 10)); err != nil {
					return errors.New("telegram durable cursor could not be saved")
				}
			}
			offset = next
		}
	}
}

// updates reads one batch of pending updates.
func (t *Telegram) updates(ctx context.Context, offset int64) ([]telegramUpdate, error) {
	query := map[string]string{"timeout": "25", "allowed_updates": `["message"]`}
	if offset > 0 {
		query["offset"] = strconv.FormatInt(offset, 10)
	}
	raw, err := t.call(ctx, "getUpdates", query)
	if err != nil {
		return nil, err
	}
	var updates []telegramUpdate
	if err = json.Unmarshal(raw, &updates); err != nil {
		return nil, err
	}
	return updates, nil
}

// translate converts one update into a normalized event.
func (t *Telegram) translate(update telegramUpdate) (Event, bool) {
	message := update.Message
	if message == nil || message.Chat == nil || message.From == nil {
		return Event{}, false
	}
	if message.From.IsBot {
		return Event{}, false
	}
	if message.Chat.Type == "channel" {
		// Broadcast channels are not an ordinary conversation surface.
		return Event{}, false
	}
	text := message.Text
	if text == "" {
		text = message.Caption
	}
	files := []File{}
	if message.Document != nil {
		files = append(files, File{ID: message.Document.FileID, Name: message.Document.FileName, MIME: message.Document.MimeType, Size: message.Document.FileSize, URL: message.Document.FileID})
	}
	if len(message.Photo) > 0 {
		largest := message.Photo[len(message.Photo)-1]
		files = append(files, File{ID: largest.FileID, Name: largest.FileUniqueID + ".jpg", MIME: largest.MimeType, Size: largest.FileSize, URL: largest.FileID})
	}
	if strings.TrimSpace(text) == "" && len(files) == 0 {
		return Event{}, false
	}
	text = clampText(text, MaxTextBytes)
	return Event{Kind: KindTelegram, EventID: strconv.FormatInt(update.UpdateID, 10), ThreadKey: strconv.FormatInt(message.Chat.ID, 10) + ":" + strconv.FormatInt(message.MessageID, 10), WorkspaceID: "", ChannelID: strconv.FormatInt(message.Chat.ID, 10), AuthorID: strconv.FormatInt(message.From.ID, 10), AuthorName: message.From.Username, Text: text, Files: files, ReceivedAt: time.Unix(message.Date, 0).UTC()}, true
}

// Post sends one reply into the recorded chat.
func (t *Telegram) Post(ctx context.Context, threadKey string, message Message) (PostResult, error) {
	chat, replyTo, ok := splitThreadKey(threadKey)
	if !ok {
		return PostResult{}, Definite("invalid telegram thread key")
	}
	text := EscapeTelegram(message.Text)
	pieces := SplitText(text, TelegramMessageLimit)
	if len(pieces) == 0 {
		pieces = []string{""}
	}
	remote := ""
	for i, piece := range pieces {
		fields := map[string]string{"chat_id": chat, "text": piece, "disable_web_page_preview": "true"}
		if i == 0 {
			fields["reply_to_message_id"] = replyTo
		}
		raw, err := t.call(ctx, "sendMessage", fields)
		if err != nil {
			return PostResult{}, Uncertain("telegram reply was not confirmed")
		}
		if i == 0 {
			var sent struct {
				MessageID int64 `json:"message_id"`
			}
			if json.Unmarshal(raw, &sent) == nil {
				remote = strconv.FormatInt(sent.MessageID, 10)
			}
		}
	}
	for _, file := range message.Files {
		if file.Open == nil || file.Size < 0 || file.Size > MaxFileBytes { return PostResult{}, Uncertain("telegram result file is outside the transfer limit") }
		if err := t.upload(ctx, chat, file); err != nil {
			return PostResult{RemoteID: remote}, err
		}
	}
	return PostResult{RemoteID: remote}, nil
}

// upload sends one result file to the recorded chat.
func (t *Telegram) upload(ctx context.Context, chat string, file OutgoingFile) error {
	reader, err := file.Open()
	if err != nil {
		return Definite("telegram file could not be opened")
	}
	defer reader.Close()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	if err = writer.WriteField("chat_id", chat); err != nil {
		return Definite("telegram file request could not be built")
	}
	part, err := writer.CreateFormFile("document", file.Name)
	if err != nil {
		return Definite("telegram file request could not be built")
	}
	size, err := io.Copy(part, io.LimitReader(reader, MaxFileBytes+1))
	if err != nil || size > MaxFileBytes { return Definite("telegram file exceeds the transfer limit or could not be read") }
	if err = writer.Close(); err != nil {
		return Definite("telegram file request could not be built")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, TelegramOrigin+"/bot"+t.token+"/sendDocument", body)
	if err != nil {
		return Definite("invalid telegram file request")
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	client, err := t.http.client(ctx, TelegramOrigin)
	if err != nil {
		return Definite("telegram origin is not approved")
	}
	response, err := client.Do(request)
	if err != nil {
		return Uncertain("telegram file upload was not confirmed")
	}
	defer response.Body.Close()
	var envelope apiResponse
	if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&envelope) != nil || !envelope.OK {
		return Uncertain("telegram file upload was not confirmed")
	}
	return nil
}

// Download opens one inbound Telegram file.
func (t *Telegram) Download(ctx context.Context, file File) (io.ReadCloser, error) {
	if file.Size > MaxFileBytes { return nil, Definite("telegram file exceeds the transfer limit") }
	raw, err := t.call(ctx, "getFile", map[string]string{"file_id": file.URL})
	if err != nil {
		return nil, Uncertain("telegram file lookup failed")
	}
	var info struct {
		FilePath string `json:"file_path"`
	}
	if err = json.Unmarshal(raw, &info); err != nil || info.FilePath == "" {
		return nil, Definite("telegram file lookup returned no path")
	}
	if strings.HasPrefix(info.FilePath, "/") || path.Clean(info.FilePath) != info.FilePath || strings.ContainsAny(info.FilePath, "\\?#\x00") {
		return nil, Definite("telegram file lookup returned an invalid path")
	}
	client, err := t.http.client(ctx, TelegramOrigin)
	if err != nil {
		return nil, Definite("telegram origin is not approved")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, TelegramOrigin+"/file/bot"+t.token+"/"+strings.TrimPrefix(info.FilePath, "/"), nil)
	if err != nil {
		return nil, Definite("invalid telegram file request")
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, Uncertain("telegram file download failed")
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, Definite("telegram file download failed with status %d", response.StatusCode)
	}
	return response.Body, nil
}

// call performs one Telegram API call. The bot token appears in the request
// path, so every error is scrubbed before it leaves this transport.
func (t *Telegram) call(ctx context.Context, method string, fields map[string]string) (json.RawMessage, error) {
	client, err := t.http.client(ctx, TelegramOrigin)
	if err != nil {
		return nil, err
	}
	var body io.Reader
	contentType := ""
	if fields != nil {
		encoded, merr := json.Marshal(fields)
		if merr != nil {
			return nil, fmt.Errorf("telegram request could not be encoded")
		}
		body = bytes.NewReader(encoded)
		contentType = "application/json"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, TelegramOrigin+"/bot"+t.token+"/"+method, body)
	if err != nil {
		return nil, fmt.Errorf("invalid telegram request")
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New("telegram provider request failed")
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("telegram response could not be read")
	}
	var envelope apiResponse
	if err = json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("telegram response was unreadable")
	}
	if response.StatusCode != http.StatusOK || !envelope.OK {
		return nil, fmt.Errorf("telegram rejected %s with status %d", method, response.StatusCode)
	}
	return envelope.Result, nil
}

// EscapeTelegram neutralizes markup in an untrusted reply. Plain text is sent
// without a parse mode, so no entity is ever interpreted.
func EscapeTelegram(text string) string {
	return text
}
