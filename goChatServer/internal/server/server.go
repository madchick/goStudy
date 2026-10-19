package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"goChatServer/internal/chat"
	"goChatServer/internal/models"
	"goChatServer/internal/storage"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	// Allow cross-origin connections from Web, Mobile, and Desktop apps
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// Server wraps the HTTP mux, SQLite store, and chat room Manager.
type Server struct {
	store   *storage.Store
	manager *chat.Manager
	mux     *http.ServeMux
}

// NewServer creates and configures the HTTP/WebSocket API server.
func NewServer(store *storage.Store, manager *chat.Manager) *Server {
	s := &Server{
		store:   store,
		manager: manager,
		mux:     http.NewServeMux(),
	}
	s.registerRoutes()
	return s
}

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("GET /api/health", s.handleHealth)

	// Session / Auth endpoints
	s.mux.HandleFunc("POST /api/auth/session", s.handleCreateSession)
	s.mux.HandleFunc("GET /api/auth/me", s.handleGetMe)
	s.mux.HandleFunc("PUT /api/auth/nickname", s.handleUpdateNickname)

	// Room management endpoints
	s.mux.HandleFunc("GET /api/rooms", s.handleListRooms)
	s.mux.HandleFunc("POST /api/rooms", s.handleCreateRoom)
	s.mux.HandleFunc("GET /api/rooms/{id}", s.handleGetRoom)
	s.mux.HandleFunc("DELETE /api/rooms/{id}", s.handleDeleteRoom)
	s.mux.HandleFunc("POST /api/rooms/{id}/verify", s.handleVerifyRoomAccess)
	s.mux.HandleFunc("GET /api/rooms/{id}/messages", s.handleGetRoomMessages)

	// Real-time WebSocket endpoint
	s.mux.HandleFunc("GET /ws/rooms/{id}", s.handleJoinRoomWS)
}

// ServeHTTP implements http.Handler with CORS support for Web, Mobile, and Desktop clients.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Room-Password")

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	s.mux.ServeHTTP(w, r)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"time":   time.Now().UTC(),
	})
}

// POST /api/auth/session
// Issues a new session token and user_id for a given nickname (and optional password).
// If the nickname already exists and the provided password matches, reclaims the existing user_id with a new token.
func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req models.CreateSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}

	nickname := strings.TrimSpace(req.Nickname)
	if nickname == "" {
		writeError(w, http.StatusBadRequest, "nickname is required")
		return
	}
	if len([]rune(nickname)) > 30 {
		writeError(w, http.StatusBadRequest, "nickname must be 30 characters or fewer")
		return
	}

	var passwordHash string
	if req.Password != "" {
		hashed, err := chat.HashPassword(req.Password)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to hash password")
			return
		}
		passwordHash = hashed
	}

	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate token")
		return
	}

	candidate := &models.UserSession{
		UserID:    uuid.NewString(),
		Nickname:  nickname,
		Token:     hex.EncodeToString(tokenBytes),
		CreatedAt: time.Now().UTC(),
	}

	session, isReclaimed, err := s.store.SaveOrReclaimSession(candidate, passwordHash, req.Password, chat.VerifyPassword)
	if err != nil {
		if errors.Is(err, storage.ErrNicknameTaken) {
			writeError(w, http.StatusConflict, "nickname is already taken or password is incorrect")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to save session")
		return
	}

	status := http.StatusCreated
	if isReclaimed {
		status = http.StatusOK
	}
	writeJSON(w, status, session)
}

// GET /api/auth/me
func (s *Server) handleGetMe(w http.ResponseWriter, r *http.Request) {
	sess, err := s.authenticate(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized: valid token required")
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

// PUT /api/auth/nickname
func (s *Server) handleUpdateNickname(w http.ResponseWriter, r *http.Request) {
	sess, err := s.authenticate(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized: valid token required")
		return
	}

	var req models.CreateSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}

	nickname := strings.TrimSpace(req.Nickname)
	if nickname == "" || len([]rune(nickname)) > 30 {
		writeError(w, http.StatusBadRequest, "nickname must be between 1 and 30 characters")
		return
	}

	if err := s.store.UpdateSessionNickname(sess.Token, nickname); err != nil {
		if errors.Is(err, storage.ErrNicknameTaken) {
			writeError(w, http.StatusConflict, "nickname is already taken")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to update nickname")
		return
	}

	sess.Nickname = nickname
	writeJSON(w, http.StatusOK, sess)
}

// GET /api/rooms?visibility=all|public|private
// Lists all rooms (including both public and private rooms by default so users can see private rooms and enter a password to join).
func (s *Server) handleListRooms(w http.ResponseWriter, r *http.Request) {
	visibility := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("visibility")))
	if visibility == "" {
		visibility = "all"
	}
	if visibility != "all" && visibility != "public" && visibility != "private" {
		writeError(w, http.StatusBadRequest, "visibility must be 'all', 'public', or 'private'")
		return
	}

	rooms, err := s.manager.ListRooms(visibility)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list rooms")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"rooms": rooms,
		"total": len(rooms),
	})
}

// POST /api/rooms
// Creates a public or private room with specified max_users (1..100) or is_unlimited (capped at 100).
func (s *Server) handleCreateRoom(w http.ResponseWriter, r *http.Request) {
	sess, err := s.authenticate(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized: valid token required")
		return
	}

	var req models.CreateRoomRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}

	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "room name is required")
		return
	}
	if len([]rune(req.Name)) > 60 {
		writeError(w, http.StatusBadRequest, "room name must be 60 characters or fewer")
		return
	}

	room, err := s.manager.CreateRoom(req, sess)
	if err != nil {
		if errors.Is(err, storage.ErrRoomNameTaken) {
			writeError(w, http.StatusConflict, "room name is already taken")
			return
		}
		if errors.Is(err, chat.ErrInvalidCapacity) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, room)
}

// GET /api/rooms/{id}
func (s *Server) handleGetRoom(w http.ResponseWriter, r *http.Request) {
	roomID := r.PathValue("id")
	room, hub, err := s.manager.GetRoom(roomID)
	if err != nil {
		if errors.Is(err, chat.ErrRoomNotFound) {
			writeError(w, http.StatusNotFound, "room not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get room")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"room":         room,
		"participants": hub.Participants(),
	})
}

// DELETE /api/rooms/{id}
func (s *Server) handleDeleteRoom(w http.ResponseWriter, r *http.Request) {
	sess, err := s.authenticate(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized: valid token required")
		return
	}

	roomID := r.PathValue("id")
	if err := s.manager.DeleteRoom(roomID, sess.UserID); err != nil {
		switch {
		case errors.Is(err, chat.ErrRoomNotFound):
			writeError(w, http.StatusNotFound, "room not found")
		case errors.Is(err, chat.ErrForbidden):
			writeError(w, http.StatusForbidden, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, "failed to delete room")
		}
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "room_id": roomID})
}

// POST /api/rooms/{id}/verify
// Pre-validates password (for private rooms) and room capacity before connecting via WebSocket.
func (s *Server) handleVerifyRoomAccess(w http.ResponseWriter, r *http.Request) {
	roomID := r.PathValue("id")

	var req models.VerifyRoomAccessRequest
	_ = json.NewDecoder(r.Body).Decode(&req)

	room, _, err := s.manager.VerifyAccess(roomID, req.Password)
	if err != nil {
		switch {
		case errors.Is(err, chat.ErrRoomNotFound):
			writeError(w, http.StatusNotFound, "room not found")
		case errors.Is(err, chat.ErrInvalidPassword):
			writeError(w, http.StatusForbidden, "invalid room password")
		case errors.Is(err, chat.ErrRoomFull):
			writeError(w, http.StatusConflict, "room is full")
		default:
			writeError(w, http.StatusInternalServerError, "failed to verify room access")
		}
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"allowed": true,
		"room":    room,
	})
}

// GET /api/rooms/{id}/messages?limit=50&before_id=0
// Fetches persisted chat history for a room. For private rooms, requires password via query `?password=` or `X-Room-Password` header.
func (s *Server) handleGetRoomMessages(w http.ResponseWriter, r *http.Request) {
	if _, err := s.authenticate(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized: valid token required")
		return
	}

	roomID := r.PathValue("id")
	room, _, err := s.manager.GetRoom(roomID)
	if err != nil {
		writeError(w, http.StatusNotFound, "room not found")
		return
	}

	if room.IsPrivate {
		password := r.Header.Get("X-Room-Password")
		if password == "" {
			password = r.URL.Query().Get("password")
		}
		if _, _, err := s.manager.VerifyAccess(roomID, password); err != nil && errors.Is(err, chat.ErrInvalidPassword) {
			writeError(w, http.StatusForbidden, "invalid room password")
			return
		}
	}

	limit := 50
	if lStr := r.URL.Query().Get("limit"); lStr != "" {
		if parsed, err := strconv.Atoi(lStr); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	var beforeID int64
	if bStr := r.URL.Query().Get("before_id"); bStr != "" {
		if parsed, err := strconv.ParseInt(bStr, 10, 64); err == nil && parsed > 0 {
			beforeID = parsed
		}
	}

	messages, err := s.store.GetRecentMessages(roomID, limit, beforeID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to fetch messages")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"room_id":  roomID,
		"messages": messages,
	})
}

// GET /ws/rooms/{id}?token=<token>&password=<password>
// Connects a Web, Mobile, or Desktop client to a room via WebSocket.
// Supports token via Authorization header (`Bearer <token>`) or `?token=` query param,
// and password via `X-Room-Password` header or `?password=` query param.
func (s *Server) handleJoinRoomWS(w http.ResponseWriter, r *http.Request) {
	sess, err := s.authenticate(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized: valid token required")
		return
	}

	roomID := r.PathValue("id")
	password := r.Header.Get("X-Room-Password")
	if password == "" {
		password = r.URL.Query().Get("password")
	}

	_, hub, err := s.manager.VerifyAccess(roomID, password)
	if err != nil {
		switch {
		case errors.Is(err, chat.ErrRoomNotFound):
			writeError(w, http.StatusNotFound, "room not found")
		case errors.Is(err, chat.ErrInvalidPassword):
			writeError(w, http.StatusForbidden, "invalid room password")
		case errors.Is(err, chat.ErrRoomFull):
			writeError(w, http.StatusConflict, "room has reached maximum capacity")
		default:
			writeError(w, http.StatusInternalServerError, "failed to join room")
		}
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	client := chat.NewClient(hub, conn, sess.UserID, sess.Nickname, roomID)
	if err := hub.Register(client); err != nil {
		// In case capacity filled up right during upgrade
		errPayload, _ := json.Marshal(models.Message{
			RoomID:    roomID,
			Type:      models.MessageTypeError,
			Content:   err.Error(),
			CreatedAt: time.Now().UTC(),
		})
		_ = conn.WriteMessage(websocket.TextMessage, errPayload)
		_ = conn.Close()
		return
	}

	go client.WritePump()
	go client.ReadPump()
}

// authenticate extracts the session token from `Authorization: Bearer <token>` or `?token=<token>`.
func (s *Server) authenticate(r *http.Request) (*models.UserSession, error) {
	var token string
	authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(authHeader), "bearer ") {
		token = strings.TrimSpace(authHeader[7:])
	}
	if token == "" {
		token = strings.TrimSpace(r.URL.Query().Get("token"))
	}
	if token == "" {
		return nil, storage.ErrNotFound
	}
	return s.store.GetSessionByToken(token)
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"error": message,
	})
}
