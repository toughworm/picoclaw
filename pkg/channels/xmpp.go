package channels

import (
	"context"
	"crypto/tls"
	"encoding/xml"
	"fmt"
	"strings"
	"sync"

	"mellium.im/sasl"
	"mellium.im/xmlstream"
	"mellium.im/xmpp"
	"mellium.im/xmpp/jid"
	"mellium.im/xmpp/stanza"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/logger"
)

type XMPPChannel struct {
	*BaseChannel
	config      config.XMPPConfig
	session     *xmpp.Session
	ctx         context.Context
	cancel      context.CancelFunc
	mu          sync.Mutex
	joinedRooms map[string]bool
}

func NewXMPPChannel(cfg config.XMPPConfig, bus *bus.MessageBus) (*XMPPChannel, error) {
	base := NewBaseChannel("xmpp", cfg, bus, cfg.AllowFrom)
	return &XMPPChannel{
		BaseChannel: base,
		config:      cfg,
		joinedRooms: make(map[string]bool),
	}, nil
}

func (c *XMPPChannel) Start(ctx context.Context) error {
	if !c.config.Enabled {
		return fmt.Errorf("xmpp channel disabled")
	}
	if c.config.Server == "" || c.config.Domain == "" || c.config.Username == "" {
		return fmt.Errorf("xmpp server, domain and username must be configured")
	}

	j, err := jid.Parse(fmt.Sprintf("%s@%s", c.config.Username, c.config.Domain))
	if err != nil {
		return fmt.Errorf("invalid jid: %w", err)
	}

	dialCtx, cancel := context.WithCancel(ctx)
	c.ctx = dialCtx
	c.cancel = cancel

	features := []xmpp.StreamFeature{}
	if c.config.UseTLS {
		features = append(features, xmpp.StartTLS(&tls.Config{
			InsecureSkipVerify: c.config.InsecureSkipVerify,
			ServerName:         c.config.Domain,
		}))
	}
	if c.config.Password != "" {
		features = append(features, xmpp.SASL("", c.config.Password, sasl.Plain))
	}
	features = append(features, xmpp.BindResource())

	session, err := xmpp.DialClientSession(dialCtx, j, features...)
	if err != nil {
		cancel()
		return fmt.Errorf("failed to dial xmpp: %w", err)
	}

	c.mu.Lock()
	c.session = session
	c.mu.Unlock()

	for _, room := range c.config.Rooms {
		if room == "" {
			continue
		}
		go c.joinRoom(room)
	}

	c.setRunning(true)
	go c.readLoop()

	return nil
}

func (c *XMPPChannel) Stop(ctx context.Context) error {
	c.setRunning(false)
	c.mu.Lock()
	if c.cancel != nil {
		c.cancel()
	}
	if c.session != nil {
		_ = c.session.Close()
	}
	c.mu.Unlock()
	return nil
}

func (c *XMPPChannel) Send(ctx context.Context, msg bus.OutboundMessage) error {
	if !c.IsRunning() {
		return fmt.Errorf("xmpp channel not running")
	}

	c.mu.Lock()
	session := c.session
	c.mu.Unlock()
	if session == nil {
		return fmt.Errorf("xmpp session not ready")
	}

	toJID, err := jid.Parse(msg.ChatID)
	if err != nil {
		return fmt.Errorf("invalid chat id: %w", err)
	}

	msgType := stanza.ChatMessage
	if c.isRoomJID(toJID) {
		msgType = stanza.GroupChatMessage
	}

	message := stanza.Message{
		To:   toJID,
		Type: msgType,
	}

	type outgoing struct {
		stanza.Message
		Body    string `xml:"body"`
		Request *struct {
			XMLName xml.Name `xml:"urn:xmpp:receipts request"`
		} `xml:"request,omitempty"`
	}

	o := outgoing{
		Message: message,
		Body:    msg.Content,
	}
	if c.config.EnableReceipts {
		o.Request = &struct {
			XMLName xml.Name `xml:"urn:xmpp:receipts request"`
		}{
			XMLName: xml.Name{Space: "urn:xmpp:receipts", Local: "request"},
		}
	}

	if err := session.Encode(ctx, o); err != nil {
		return fmt.Errorf("failed to send xmpp message: %w", err)
	}

	return nil
}

func (c *XMPPChannel) readLoop() {
	c.mu.Lock()
	session := c.session
	c.mu.Unlock()
	if session == nil {
		return
	}

	type inbound struct {
		stanza.Message
		Body    string `xml:"body"`
		Request *struct {
			XMLName xml.Name `xml:"urn:xmpp:receipts request"`
		} `xml:"request"`
	}

	err := session.Serve(xmpp.HandlerFunc(func(t xmlstream.TokenReadEncoder, start *xml.StartElement) error {
		if start.Name.Local != "message" {
			return nil
		}

		var msg inbound
		dec := xml.NewTokenDecoder(t)
		if err := dec.DecodeElement(&msg, start); err != nil {
			logger.ErrorCF("xmpp", "decode message error", map[string]interface{}{
				"error": err.Error(),
			})
			return nil
		}

		if msg.Body == "" {
			return nil
		}

		senderID := c.buildSenderID(msg.From)
		if !c.IsAllowed(senderID) {
			return nil
		}

		chatID := c.buildChatID(msg.Message)
		if chatID == "" {
			return nil
		}

		metadata := map[string]string{}
		if msg.ID != "" {
			metadata["xmpp_id"] = msg.ID
		}
		if msg.Type == stanza.GroupChatMessage {
			metadata["xmpp_type"] = "groupchat"
		} else {
			metadata["xmpp_type"] = "chat"
		}

		if msg.Request != nil && msg.ID != "" && c.config.EnableReceipts {
			go c.sendReceipt(msg.Message)
		}

		c.HandleMessage(senderID, chatID, msg.Body, nil, metadata)
		return nil
	}))

	if err != nil && c.ctx.Err() == nil {
		logger.ErrorCF("xmpp", "session serve error", map[string]interface{}{
			"error": err.Error(),
		})
	}
}

func (c *XMPPChannel) sendReceipt(m stanza.Message) {
	c.mu.Lock()
	session := c.session
	c.mu.Unlock()
	if session == nil {
		return
	}

	type receipt struct {
		stanza.Message
		Received struct {
			XMLName xml.Name `xml:"urn:xmpp:receipts received"`
			ID      string   `xml:"id,attr"`
		} `xml:"received"`
	}

	r := receipt{
		Message: stanza.Message{
			To:   m.From,
			Type: m.Type,
		},
	}
	r.Received.ID = m.ID

	_ = session.Encode(context.Background(), r)
}

func (c *XMPPChannel) buildSenderID(j jid.JID) string {
	bare := j.Bare().String()
	resource := j.Resourcepart()
	if resource == "" {
		return bare
	}
	return bare + "|" + resource
}

func (c *XMPPChannel) buildChatID(m stanza.Message) string {
	if m.Type == stanza.GroupChatMessage {
		return m.From.Bare().String()
	}
	return m.From.Bare().String()
}

func (c *XMPPChannel) isRoomJID(j jid.JID) bool {
	for _, room := range c.config.Rooms {
		if room == "" {
			continue
		}
		if strings.EqualFold(j.Bare().String(), room) {
			return true
		}
	}
	return false
}

func (c *XMPPChannel) effectiveResource() string {
	if strings.TrimSpace(c.config.Resource) != "" {
		return c.config.Resource
	}
	return "picoclaw"
}

func (c *XMPPChannel) joinRoom(room string) {
	c.mu.Lock()
	session := c.session
	ctx := c.ctx
	c.mu.Unlock()
	if session == nil || ctx == nil {
		return
	}

	roomJID, err := jid.Parse(room + "/" + c.roomNickname())
	if err != nil {
		logger.ErrorCF("xmpp", "invalid room jid", map[string]interface{}{
			"room":  room,
			"error": err.Error(),
		})
		return
	}

	type mucPresence struct {
		stanza.Presence
		X struct {
			XMLName xml.Name `xml:"http://jabber.org/protocol/muc x"`
		} `xml:"x"`
	}

	p := mucPresence{
		Presence: stanza.Presence{
			To: roomJID,
		},
	}

	if err := session.Encode(ctx, p); err != nil {
		logger.ErrorCF("xmpp", "join room failed", map[string]interface{}{
			"room":  room,
			"error": err.Error(),
		})
		return
	}

	c.mu.Lock()
	c.joinedRooms[roomJID.Bare().String()] = true
	c.mu.Unlock()
}

func (c *XMPPChannel) roomNickname() string {
	n := strings.TrimSpace(c.config.Nickname)
	if n == "" {
		return "PicoClaw"
	}
	return n
}
