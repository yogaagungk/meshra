package connectors

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

// SlackOrigin is the exact API origin. The operator must grant it for the
// connector before any Slack call is attempted.
const SlackOrigin = "https://slack.com"

// SlackFilesOrigin is the exact origin of private file downloads. Slack
// refuses to send the bot token to any other host.
const SlackFilesOrigin = "https://files.slack.com"


// SlackMessageLimit is the reply size accepted by chat.postMessage.
const SlackMessageLimit = 3000

// Slack implements the Slack Socket Mode transport. Socket Mode is an outbound
// websocket: no inbound listener is opened.
type Slack struct {
	cfg    Config
	api    *slack.Client
	socket *socketmode.Client
	http *providerHTTP
	mu sync.Mutex
	cancel context.CancelFunc
}

// NewSlack builds a Slack transport from resolved credentials.
func NewSlack(ctx context.Context, cfg Config) (*Slack, error) {
	if cfg.Token == "" || cfg.AppToken == "" {
		return nil, errors.New("slack connector requires a bot token and an app-level token")
	}
	if cfg.Egress.Client == nil || cfg.Egress.Dial == nil {
		return nil, errors.New("slack connector requires approved egress")
	}
	network := &providerHTTP{egress: cfg.Egress}
	client, err := network.client(ctx, SlackOrigin)
	if err != nil {
		return nil, err
	}
	api := slack.New(cfg.Token, slack.OptionAPIURL(SlackOrigin+"/api/"), slack.OptionHTTPClient(client), slack.OptionAppLevelToken(cfg.AppToken))
	dialer := socketDialer(cfg.Egress, SocketHosts(KindSlack))
	return &Slack{cfg: cfg, api: api, socket: socketmode.New(api, socketmode.OptionDialer(dialer)), http: network}, nil
}

// Kind reports the provider.
func (s *Slack) Kind() Kind { return KindSlack }

// Close cancels the active receive loop. Receive joins the SDK runner before
// returning, so callers can wait on their own receive goroutine.
func (s *Slack) Close() error {
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil { cancel() }
	return nil
}

// Test verifies the bot token and the app-level token without sending a message.
func (s *Slack) Test(ctx context.Context) error {
	client, err := s.http.client(ctx, SlackOrigin)
	if err != nil {
		return err
	}
	probe := slack.New(s.cfg.Token, slack.OptionAPIURL(SlackOrigin+"/api/"), slack.OptionHTTPClient(client))
	if _, err = probe.AuthTestContext(ctx); err != nil {
		return Definite("slack credential check failed")
	}
	return nil
}

// Receive runs the Socket Mode loop until the context is cancelled.
func (s *Slack) Receive(ctx context.Context, sink Sink) error {
	ctx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.cancel = cancel
	s.mu.Unlock()
	errs := make(chan error, 1)
	go func() { errs <- s.socket.RunContext(ctx) }()
	defer func() {
		cancel()
		<-errs
		s.mu.Lock()
		s.cancel = nil
		s.mu.Unlock()
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errs:
			// Preserve one completion for the deferred join.
			errs <- err
			if err == nil || errors.Is(err, context.Canceled) { return ctx.Err() }
			return Uncertain("slack socket mode stopped")
		case event, ok := <-s.socket.Events:
			if !ok {
				return Uncertain("slack socket mode channel closed")
			}
			if event.Type != socketmode.EventTypeEventsAPI {
				continue
			}
			parsed, ok := event.Data.(*slackevents.EventsAPIEvent)
			if !ok {
				continue
			}
			for _, translated := range s.translate(parsed) {
				if !sink(translated) { return errors.New("slack event is awaiting durable acceptance") }
			}
			if event.Request != nil && event.Request.EnvelopeID != "" {
				if err := s.socket.AckCtx(ctx, event.Request.EnvelopeID, nil); err != nil {
					return errors.New("slack receipt acknowledgement failed")
				}
			}
		}
	}
}

// translate converts Slack events into normalized messages. Bot traffic and
// message subtypes the operator did not ask for are ignored.
func (s *Slack) translate(event *slackevents.EventsAPIEvent) []Event {
	callback, ok := event.Data.(*slackevents.EventsAPICallbackEvent)
	if !ok || callback == nil {
		return nil
	}
	var message *slackevents.MessageEvent
	switch inner := event.InnerEvent.Data.(type) {
	case *slackevents.MessageEvent:
		if inner == nil || inner.ChannelType != "im" && inner.ChannelType != "mpim" { return nil }
		message = inner
	case *slackevents.AppMentionEvent:
		if inner == nil { return nil }
		message = &slackevents.MessageEvent{User: inner.User, Text: inner.Text, TimeStamp: inner.TimeStamp, ThreadTimeStamp: inner.ThreadTimeStamp, Channel: inner.Channel, BotID: inner.BotID, Message: &slack.Msg{Files: inner.Files}}
	default:
		return nil
	}
	if message.BotID != "" || message.SubType == "bot_message" {
		return nil
	}
	if message.SubType != "" && message.SubType != "file_share" {
		return nil
	}
	if message.User == "" || message.Channel == "" {
		return nil
	}
	anchor := message.ThreadTimeStamp
	if anchor == "" {
		anchor = message.TimeStamp
	}
	if anchor == "" {
		return nil
	}
	text := clampText(message.Text, MaxTextBytes)
	out := Event{Kind: KindSlack, EventID: callback.EventID, ThreadKey: message.Channel + ":" + anchor, WorkspaceID: callback.TeamID, ChannelID: message.Channel, AuthorID: message.User, Text: text, ReceivedAt: time.Now().UTC()}
	if out.EventID == "" {
		out.EventID = message.Channel + ":" + message.TimeStamp
	}
	if message.Message != nil {
		for i, file := range message.Message.Files {
			if i >= MaxFiles {
				break
			}
			if file.IsExternal || file.URLPrivate == "" {
				continue
			}
			out.Files = append(out.Files, File{ID: file.ID, Name: file.Name, MIME: file.Mimetype, Size: int64(file.Size), URL: file.URLPrivate})
		}
	}
	return []Event{out}
}

// Post sends one reply into the recorded thread.
func (s *Slack) Post(ctx context.Context, threadKey string, message Message) (PostResult, error) {
	channel, thread, ok := splitThreadKey(threadKey)
	if !ok {
		return PostResult{}, Definite("invalid slack thread key")
	}
	text := EscapeSlack(message.Text)
	pieces := SplitText(text, SlackMessageLimit)
	if len(pieces) == 0 {
		pieces = []string{""}
	}
	remote := ""
	for i, piece := range pieces {
		options := []slack.MsgOption{slack.MsgOptionText(piece, false), slack.MsgOptionTS(thread)}
		_, timestamp, err := s.api.PostMessageContext(ctx, channel, options...)
		if err != nil {
			return PostResult{}, Uncertain("slack reply was not confirmed")
		}
		if i == 0 {
			remote = timestamp
		}
	}
	for _, file := range message.Files {
		if err := s.upload(ctx, channel, thread, file); err != nil {
			return PostResult{RemoteID: remote}, err
		}
	}
	return PostResult{RemoteID: remote}, nil
}

// upload sends one result file into the recorded thread.
func (s *Slack) upload(ctx context.Context, channel, thread string, file OutgoingFile) error {
	if file.Size < 1 || file.Size > MaxFileBytes { return Definite("slack file size is outside the transfer limit") }
	if file.Open == nil {
		return Definite("slack file has no readable content")
	}
	reader, err := file.Open()
	if err != nil {
		return Definite("slack file could not be opened")
	}
	defer reader.Close()
	upload, err := s.api.GetUploadURLExternalContext(ctx, slack.GetUploadURLExternalParameters{FileName: file.Name, FileSize: int(file.Size)})
	if err != nil { return Uncertain("slack file upload setup was not confirmed") }
	if upload == nil || upload.FileID == "" || !permittedURL(upload.UploadURL, SlackFilesOrigin) {
		return Uncertain("slack returned an unapproved file upload origin")
	}
	client, err := s.http.client(ctx, SlackFilesOrigin)
	if err != nil { return Uncertain("slack file upload origin is not approved") }
	fileAPI := slack.New(s.cfg.Token, slack.OptionHTTPClient(client))
	if err = fileAPI.UploadToURL(ctx, slack.UploadToURLParameters{UploadURL: upload.UploadURL, Reader: io.LimitReader(reader, MaxFileBytes+1), Filename: file.Name}); err != nil {
		return Uncertain("slack file upload was not confirmed")
	}
	if _, err = s.api.CompleteUploadExternalContext(ctx, slack.CompleteUploadExternalParameters{Files: []slack.FileSummary{{ID: upload.FileID, Title: file.Name}}, Channel: channel, ThreadTimestamp: thread}); err != nil {
		return Uncertain("slack file publication was not confirmed")
	}
	return nil
}

// Download opens one inbound Slack file. Only provider owned origins are read.
func (s *Slack) Download(ctx context.Context, file File) (io.ReadCloser, error) {
	if file.Size > MaxFileBytes || !permittedURL(file.URL, SlackFilesOrigin) {
		return nil, Definite("slack file origin is not permitted")
	}
	client, err := s.http.client(ctx, SlackFilesOrigin)
	if err != nil {
		return nil, Definite("slack file origin is not approved")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, file.URL, nil)
	if err != nil {
		return nil, Definite("invalid slack file request")
	}
	request.Header.Set("Authorization", "Bearer "+s.cfg.Token)
	response, err := client.Do(request)
	if err != nil {
		return nil, Uncertain("slack file download failed")
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, Definite("slack file download failed with status %d", response.StatusCode)
	}
	return response.Body, nil
}

// splitThreadKey splits a recorded Slack thread key.
func splitThreadKey(key string) (string, string, bool) {
	index := strings.Index(key, ":")
	if index <= 0 || index == len(key)-1 {
		return "", "", false
	}
	return key[:index], key[index+1:], true
}

// EscapeSlack neutralizes markup and mentions in an untrusted reply. Slack
// only links a mention through the <@U…> form, which the angle brackets
// escaping removes.
func EscapeSlack(text string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(text)
}
