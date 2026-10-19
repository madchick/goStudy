package models

import "time"

const MaxRoomCapacity = 100

// UserSession represents an authenticated user session.
type UserSession struct {
	UserID    string    `json:"user_id"`
	Nickname  string    `json:"nickname"`
	Token     string    `json:"token,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Room represents a chat room (public or private).
type Room struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	IsPrivate      bool      `json:"is_private"`
	PasswordHash   string    `json:"-"`
	MaxUsers       int       `json:"max_users"`       // 1..100 (unlimited defaults to 100)
	IsUnlimited    bool      `json:"is_unlimited"`    // true if created with unlimited option (capped at 100)
	CurrentUsers   int       `json:"current_users"`   // live count of connected users
	CreatedBy      string    `json:"created_by"`      // UserID of the creator
	CreatorName    string    `json:"creator_name"`    // Nickname of the creator
	CreatedAt      time.Time `json:"created_at"`
}

// MessageType defines the type of WebSocket/Chat event.
type MessageType string

const (
	MessageTypeChat      MessageType = "chat"
	MessageTypeJoin      MessageType = "join"
	MessageTypeLeave     MessageType = "leave"
	MessageTypeRoomState MessageType = "room_state"
	MessageTypeError     MessageType = "error"
)

// Message represents a chat or system message in a room.
type Message struct {
	ID        int64       `json:"id,omitempty"`
	RoomID    string      `json:"room_id"`
	UserID    string      `json:"user_id,omitempty"`
	Nickname  string      `json:"nickname,omitempty"`
	Type      MessageType `json:"type"`
	Content   string      `json:"content"`
	CreatedAt time.Time   `json:"created_at"`
}

// Participant represents an active user inside a chat room.
type Participant struct {
	UserID   string `json:"user_id"`
	Nickname string `json:"nickname"`
}

// RoomStatePayload is sent when a user joins or when room state updates.
type RoomStatePayload struct {
	Type         MessageType   `json:"type"`
	RoomID       string        `json:"room_id"`
	CurrentUsers int           `json:"current_users"`
	MaxUsers     int           `json:"max_users"`
	Participants []Participant `json:"participants"`
}

// CreateSessionRequest is the request payload for POST /api/auth/session and PUT /api/auth/nickname.
type CreateSessionRequest struct {
	Nickname string `json:"nickname"`
	Password string `json:"password,omitempty"`
}

// CreateRoomRequest is the request payload for POST /api/rooms.
type CreateRoomRequest struct {
	Name        string `json:"name"`
	IsPrivate   bool   `json:"is_private"`
	Password    string `json:"password,omitempty"`
	MaxUsers    int    `json:"max_users"`    // If 0 or IsUnlimited is true, set to 100
	IsUnlimited bool   `json:"is_unlimited"` // When true, MaxUsers is set to 100
}

// VerifyRoomAccessRequest is the request payload for POST /api/rooms/{id}/verify.
type VerifyRoomAccessRequest struct {
	Password string `json:"password,omitempty"`
}

// IncomingWSMessage represents a message sent from a client over WebSocket.
type IncomingWSMessage struct {
	Type    MessageType `json:"type"`
	Content string      `json:"content"`
}
