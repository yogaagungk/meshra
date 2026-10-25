package connectors

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// DiscordOrigin is the exact Discord API origin. The operator must grant it
// for the connector before any Discord call is attempted.
const DiscordOrigin = "https://discord.com"


// DiscordMessageLimit is the reply size accepted by the Discord REST API.
const DiscordMessageLimit = 2000

// Discord implements the Discord Gateway transport. The gateway is an outbound
// websocket: no inbound listener is opened.
type Discord struct {
	cfg     Config
	session *discordgo.Session
	http *providerHTTP
}

// NewDiscord builds a Discord transport from a resolved bot token.
func NewDiscord(ctx context.Context, cfg Config) (*Discord, error) {
	if cfg.Token == "" {
		return nil, errors.New("discord connector requires a bot token")
	}
	if cfg.Egress.Client == nil || cfg.Egress.Dial == nil {
		return nil, errors.New("discord connector requires approved egress")
	}
	network := &providerHTTP{egress: cfg.Egress}
	client, err := network.client(ctx, DiscordOrigin)
	if err != nil {
		return nil, err
	}
	session, err := discordgo.New("Bot " + cfg.Token)
	if err != nil {
		return nil, Definite("discord session could not be created")
	}
	session.Client = client
	session.Dialer = socketDialer(cfg.Egress, SocketHosts(KindDiscord))
	session.SyncEvents = true
	session.ShouldReconnectOnError = false
	session.ShouldRetryOnRateLimit = false
	session.MaxRestRetries = 0
	// Minimal intents: guild messages, direct messages and message content.
	session.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentsDirectMessages | discordgo.IntentsMessageContent
	return &Discord{cfg: cfg, session: session, http: network}, nil
}

// Kind reports the provider.
func (d *Discord) Kind() Kind { return KindDiscord }

// Close ends the gateway session.
func (d *Discord) Close() error {
	if d.session == nil {
		return nil
	}
	return d.session.Close()
}

// Test verifies the bot token without sending a message.
func (d *Discord) Test(ctx context.Context) error {
	client, err := d.http.client(ctx, DiscordOrigin)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, DiscordOrigin+"/api/v10/users/@me", nil)
	if err != nil {
		return Definite("invalid discord probe request")
	}
	request.Header.Set("Authorization", "Bot "+d.cfg.Token)
	response, err := client.Do(request)
	if err != nil {
		return Uncertain("discord credential check failed")
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return Definite("discord credential was rejected")
	}
	if response.StatusCode != http.StatusOK {
		return Definite("discord credential check failed with status %d", response.StatusCode)
	}
	return nil
}

// Receive runs the gateway until the context is cancelled.
func (d *Discord) Receive(ctx context.Context, sink Sink) error {
	disconnected := make(chan struct{}, 1)
	removeDisconnect := d.session.AddHandler(func(_ *discordgo.Session, _ *discordgo.Disconnect) {
		select { case disconnected <- struct{}{}: default: }
	})
	defer removeDisconnect()
	remove := d.session.AddHandler(func(_ *discordgo.Session, message *discordgo.MessageCreate) {
		translated, ok := d.translate(message.Message)
		if !ok { return }
		// Synchronous dispatch supplies backpressure instead of unbounded SDK
		// handler goroutines or dropping a rejected durable receipt.
		for ctx.Err() == nil && !sink(translated) {
			if waitContext(ctx, time.Second) != nil { return }
		}
	})
	defer remove()
	baseDial := d.session.Dialer.NetDialTLSContext
	d.session.Dialer.NetDialTLSContext = func(callCtx context.Context, network, address string) (net.Conn, error) {
		dialCtx, cancel := context.WithCancel(callCtx)
		stop := context.AfterFunc(ctx, cancel)
		defer stop()
		defer cancel()
		return baseDial(dialCtx, network, address)
	}
	defer func() { d.session.Dialer.NetDialTLSContext = baseDial }()
	if err := d.session.Open(); err != nil {
		return Uncertain("discord gateway could not be opened")
	}
	defer d.session.Close()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-disconnected:
			return Uncertain("discord gateway connection stopped")
	}
}

// translate converts a Discord message into a normalized event. Bot and
// webhook traffic is ignored so the connector never answers itself.
func (d *Discord) translate(message *discordgo.Message) (Event, bool) {
	if message == nil || message.Author == nil {
		return Event{}, false
	}
	if message.Author.Bot || message.WebhookID != "" {
		return Event{}, false
	}
	if message.Author.ID == "" || message.ChannelID == "" || message.ID == "" {
		return Event{}, false
	}
	text := clampText(message.Content, MaxTextBytes)
	anchor := message.ID
	if message.MessageReference != nil && message.MessageReference.MessageID != "" { anchor = message.MessageReference.MessageID }
	out := Event{Kind: KindDiscord, EventID: message.ID, ThreadKey: message.ChannelID + ":" + anchor, WorkspaceID: message.GuildID, ChannelID: message.ChannelID, AuthorID: message.Author.ID, AuthorName: clampText(message.Author.Username, MaxAuthorBytes), Text: text, ReceivedAt: message.Timestamp.UTC()}
	if out.ReceivedAt.IsZero() {
		out.ReceivedAt = time.Now().UTC()
	}
	for i, attachment := range message.Attachments {
		if i >= MaxFiles {
			break
		}
		if attachment == nil || attachment.URL == "" {
			continue
		}
		out.Files = append(out.Files, File{ID: attachment.ID, Name: attachment.Filename, MIME: attachment.ContentType, Size: int64(attachment.Size), URL: attachment.URL})
	}
	return out, true
}

// Post sends one reply into the recorded thread.
func (d *Discord) Post(ctx context.Context, threadKey string, message Message) (PostResult, error) {
	channel, reference, ok := splitThreadKey(threadKey)
	if !ok {
		return PostResult{}, Definite("invalid discord thread key")
	}
	text := EscapeDiscord(message.Text)
	pieces := SplitText(text, DiscordMessageLimit)
	if len(pieces) == 0 {
		pieces = []string{""}
	}
	files := make([]*discordgo.File, 0, len(message.Files))
	for _, file := range message.Files {
		if file.Open == nil || file.Size < 0 || file.Size > MaxFileBytes { return PostResult{}, Definite("discord file is outside the transfer limit") }
		reader, err := file.Open()
		if err != nil { return PostResult{}, Definite("discord file could not be opened") }
		defer reader.Close()
		files = append(files, &discordgo.File{Name: file.Name, ContentType: file.MIME, Reader: io.LimitReader(reader, MaxFileBytes+1)})
	}
	sent, err := d.session.ChannelMessageSendComplex(channel, &discordgo.MessageSend{Content: pieces[0], Files: files, AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}, Reference: &discordgo.MessageReference{MessageID: reference, ChannelID: channel}}, discordgo.WithContext(ctx))
	if err != nil {
		return PostResult{}, Uncertain("discord reply was not confirmed")
	}
	for _, piece := range pieces[1:] {
		if _, err = d.session.ChannelMessageSendComplex(channel, &discordgo.MessageSend{Content: piece, AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}, Reference: &discordgo.MessageReference{MessageID: reference, ChannelID: channel}}, discordgo.WithContext(ctx)); err != nil {
			return PostResult{RemoteID: sent.ID}, Uncertain("discord reply part was not confirmed")
		}
	}
	return PostResult{RemoteID: sent.ID}, nil
}

// Download opens one inbound Discord attachment. Only the exact origin of the
// attachment URL is read, and only when the operator granted it.
func (d *Discord) Download(ctx context.Context, file File) (io.ReadCloser, error) {
	parsed, err := url.Parse(file.URL)
	if err != nil || file.Size > MaxFileBytes || !permittedURL(file.URL, "https://cdn.discordapp.net") && !permittedURL(file.URL, "https://media.discordapp.net") {
		return nil, Definite("discord attachment origin is not permitted")
	}
	client, err := d.http.client(ctx, "https://"+parsed.Host)
	if err != nil {
		return nil, Definite("discord attachment origin is not approved")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, file.URL, nil)
	if err != nil {
		return nil, Definite("invalid discord attachment request")
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, Uncertain("discord attachment download failed")
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, Definite("discord attachment download failed with status %d", response.StatusCode)
	}
	return response.Body, nil
}

// EscapeDiscord neutralizes markdown and mentions in an untrusted reply.
func EscapeDiscord(text string) string {
	replacer := strings.NewReplacer("\\", "\\\\", "*", "\\*", "_", "\\_", "~", "\\~", "`", "\\`", ">", "\\>", "#", "\\#")
	return replacer.Replace(text)
}
