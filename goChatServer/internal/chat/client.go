package chat

import (
	"encoding/json"
	"log"
	"strings"
	"time"

	"goChatServer/internal/models"

	"github.com/gorilla/websocket"
)

const (
	// Time allowed to write a message to the peer.
	writeWait = 10 * time.Second

	// Time allowed to read the next pong message from the peer.
	pongWait = 60 * time.Second

	// Send pings to peer with this period. Must be less than pongWait.
	pingPeriod = (pongWait * 9) / 10

	// Maximum message size allowed from peer (8 KB).
	maxMessageSize = 8192
)

// Client is a middleman between the websocket connection and the RoomHub.
type Client struct {
	hub  *RoomHub
	conn *websocket.Conn
	send chan []byte

	UserID   string
	Nickname string
	RoomID   string
}

// NewClient creates a new WebSocket client bound to a RoomHub.
func NewClient(hub *RoomHub, conn *websocket.Conn, userID, nickname, roomID string) *Client {
	return &Client{
		hub:      hub,
		conn:     conn,
		send:     make(chan []byte, 256),
		UserID:   userID,
		Nickname: nickname,
		RoomID:   roomID,
	}
}

// ReadPump pumps messages from the websocket connection to the RoomHub.
func (c *Client) ReadPump() {
	defer func() {
		c.hub.unregister <- c
		_ = c.conn.Close()
	}()

	c.conn.SetReadLimit(maxMessageSize)
	_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for {
		_, rawMessage, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure, websocket.CloseNormalClosure) {
				log.Printf("[WS] unexpected close error (room=%s, user=%s): %v", c.RoomID, c.UserID, err)
			}
			break
		}

		// Parse JSON message or fallback to plain text content
		var incoming models.IncomingWSMessage
		if err := json.Unmarshal(rawMessage, &incoming); err != nil {
			incoming = models.IncomingWSMessage{
				Type:    models.MessageTypeChat,
				Content: string(rawMessage),
			}
		}

		content := strings.TrimSpace(incoming.Content)
		if content == "" {
			continue
		}

		msg := &models.Message{
			RoomID:    c.RoomID,
			UserID:    c.UserID,
			Nickname:  c.Nickname,
			Type:      models.MessageTypeChat,
			Content:   content,
			CreatedAt: time.Now().UTC(),
		}

		c.hub.broadcast <- msg
	}
}

// WritePump pumps messages from the RoomHub to the websocket connection.
func (c *Client) WritePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		_ = c.conn.Close()
	}()

	for {
		select {
		case message, ok := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				// The hub closed the channel.
				_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			if err := c.conn.WriteMessage(websocket.TextMessage, message); err != nil {
				return
			}

		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
