package chat

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"goChatServer/internal/models"
	"goChatServer/internal/storage"

	"github.com/google/uuid"
)

var (
	ErrRoomNotFound    = errors.New("room not found")
	ErrRoomFull        = errors.New("room has reached maximum capacity")
	ErrInvalidPassword = errors.New("invalid password for private room")
	ErrInvalidCapacity = errors.New("max_users must be between 1 and 100")
	ErrForbidden       = errors.New("only the room creator can perform this action")
)

type registerRequest struct {
	client *Client
	errCh  chan error
}

// RoomHub manages active WebSocket clients and message broadcasting for a single room.
type RoomHub struct {
	room       *models.Room
	store      *storage.Store
	onEmpty    func(roomID string)
	mu         sync.RWMutex
	clients    map[*Client]bool
	register   chan registerRequest
	unregister chan *Client
	broadcast  chan *models.Message
	stopCh     chan struct{}
	stopOnce   sync.Once
}

func newRoomHub(room *models.Room, store *storage.Store, onEmpty func(roomID string)) *RoomHub {
	return &RoomHub{
		room:       room,
		store:      store,
		onEmpty:    onEmpty,
		clients:    make(map[*Client]bool),
		register:   make(chan registerRequest),
		unregister: make(chan *Client),
		broadcast:  make(chan *models.Message, 256),
		stopCh:     make(chan struct{}),
	}
}

// Run starts the event loop for a RoomHub.
func (h *RoomHub) Run() {
	for {
		select {
		case req := <-h.register:
			h.mu.Lock()
			if len(h.clients) >= h.room.MaxUsers {
				h.mu.Unlock()
				req.errCh <- ErrRoomFull
				continue
			}
			h.clients[req.client] = true
			currentCount := len(h.clients)
			h.mu.Unlock()

			req.errCh <- nil

			// Enqueue to memory queue (flushed every 0.5s in bulk transaction) & broadcast join event
			joinMsg := &models.Message{
				RoomID:    h.room.ID,
				UserID:    req.client.UserID,
				Nickname:  req.client.Nickname,
				Type:      models.MessageTypeJoin,
				Content:   fmt.Sprintf("%s님이 입장했습니다.", req.client.Nickname),
				CreatedAt: time.Now().UTC(),
			}
			h.store.EnqueueMessage(joinMsg)
			h.broadcastJSON(joinMsg)
			h.broadcastRoomState(currentCount)

		case client := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.clients[client]; !ok {
				h.mu.Unlock()
				continue
			}
			delete(h.clients, client)
			close(client.send)
			currentCount := len(h.clients)
			h.mu.Unlock()

			// If the last user left or disconnected, destroy the room immediately
			if currentCount == 0 {
				h.Stop()
				if h.onEmpty != nil {
					h.onEmpty(h.room.ID)
				}
				return
			}

			leaveMsg := &models.Message{
				RoomID:    h.room.ID,
				UserID:    client.UserID,
				Nickname:  client.Nickname,
				Type:      models.MessageTypeLeave,
				Content:   fmt.Sprintf("%s님이 퇴장했습니다.", client.Nickname),
				CreatedAt: time.Now().UTC(),
			}
			h.store.EnqueueMessage(leaveMsg)
			h.broadcastJSON(leaveMsg)
			h.broadcastRoomState(currentCount)

		case message := <-h.broadcast:
			h.store.EnqueueMessage(message)
			h.broadcastJSON(message)

		case <-h.stopCh:
			h.mu.Lock()
			for client := range h.clients {
				delete(h.clients, client)
				close(client.send)
			}
			h.mu.Unlock()
			return
		}
	}
}

func (h *RoomHub) broadcastJSON(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		log.Printf("[Hub] marshal error: %v", err)
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	for client := range h.clients {
		select {
		case client.send <- data:
		default:
			close(client.send)
			delete(h.clients, client)
		}
	}
}

func (h *RoomHub) broadcastRoomState(currentCount int) {
	participants := h.Participants()
	state := models.RoomStatePayload{
		Type:         models.MessageTypeRoomState,
		RoomID:       h.room.ID,
		CurrentUsers: currentCount,
		MaxUsers:     h.room.MaxUsers,
		Participants: participants,
	}
	h.broadcastJSON(state)
}

// Register attempts to add a client to the room, enforcing the room's MaxUsers limit atomically.
func (h *RoomHub) Register(client *Client) error {
	req := registerRequest{
		client: client,
		errCh:  make(chan error, 1),
	}
	select {
	case h.register <- req:
		return <-req.errCh
	case <-h.stopCh:
		return ErrRoomNotFound
	}
}

// CurrentUsers returns the live number of connected clients in the room.
func (h *RoomHub) CurrentUsers() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// Participants returns the list of currently connected users in the room.
func (h *RoomHub) Participants() []models.Participant {
	h.mu.RLock()
	defer h.mu.RUnlock()
	list := make([]models.Participant, 0, len(h.clients))
	for c := range h.clients {
		list = append(list, models.Participant{
			UserID:   c.UserID,
			Nickname: c.Nickname,
		})
	}
	return list
}

// Stop closes all client connections in this room hub.
func (h *RoomHub) Stop() {
	h.stopOnce.Do(func() {
		close(h.stopCh)
	})
}

// HashPassword generates a salted SHA-256 hash formatted as "<hex_salt>$<hex_sha256_hash>".
// This provides lightweight, constant-time verifiable password hashing with minimal CPU overhead.
func HashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	saltHex := hex.EncodeToString(salt)
	sum := sha256.Sum256(append(salt, []byte(password)...))
	hashHex := hex.EncodeToString(sum[:])
	return saltHex + "$" + hashHex, nil
}

// VerifyPassword compares a plaintext password against a stored "<hex_salt>$<hex_sha256_hash>" string
// using constant-time comparison.
func VerifyPassword(storedHash, password string) bool {
	parts := strings.SplitN(storedHash, "$", 2)
	if len(parts) != 2 {
		return false
	}
	salt, err := hex.DecodeString(parts[0])
	if err != nil {
		return false
	}
	expectedHash, err := hex.DecodeString(parts[1])
	if err != nil {
		return false
	}
	actualSum := sha256.Sum256(append(salt, []byte(password)...))
	return subtle.ConstantTimeCompare(expectedHash, actualSum[:]) == 1
}

// Manager coordinates all rooms and their hubs, synchronized with SQLite storage.
type Manager struct {
	store *storage.Store
	mu    sync.RWMutex
	hubs  map[string]*RoomHub
}

// NewManager initializes the room manager and loads existing rooms from SQLite.
func NewManager(store *storage.Store) (*Manager, error) {
	m := &Manager{
		store: store,
		hubs:  make(map[string]*RoomHub),
	}

	rooms, err := store.ListRooms("all")
	if err != nil {
		return nil, fmt.Errorf("failed to load rooms from storage: %w", err)
	}

	for _, r := range rooms {
		hub := newRoomHub(r, store, m.removeEmptyRoom)
		m.hubs[r.ID] = hub
		go hub.Run()
	}

	return m, nil
}

func (m *Manager) removeEmptyRoom(roomID string) {
	m.mu.Lock()
	delete(m.hubs, roomID)
	m.mu.Unlock()

	if err := m.store.DeleteRoom(roomID); err != nil && !errors.Is(err, storage.ErrNotFound) {
		log.Printf("[Manager] failed to delete empty room %s: %v", roomID, err)
	}
}

// Shutdown stops all active room hubs, disconnects clients, and deletes all rooms from storage.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	hubs := make([]*RoomHub, 0, len(m.hubs))
	for id, hub := range m.hubs {
		hubs = append(hubs, hub)
		delete(m.hubs, id)
	}
	m.mu.Unlock()

	for _, hub := range hubs {
		hub.Stop()
	}

	if err := m.store.ClearAllRooms(); err != nil {
		log.Printf("[Manager] failed to clear rooms on shutdown: %v", err)
	}
}

// NormalizeCapacity validates and normalizes room capacity according to the spec:
// - If isUnlimited is true OR maxUsers == 0 -> sets MaxUsers = 100 (models.MaxRoomCapacity) and IsUnlimited = true.
// - Otherwise, maxUsers must be between 1 and 100.
func NormalizeCapacity(maxUsers int, isUnlimited bool) (int, bool, error) {
	if isUnlimited || maxUsers == 0 {
		return models.MaxRoomCapacity, true, nil
	}
	if maxUsers < 1 || maxUsers > models.MaxRoomCapacity {
		return 0, false, ErrInvalidCapacity
	}
	return maxUsers, false, nil
}

// CreateRoom creates a new public or private room, persists it, and launches its RoomHub.
func (m *Manager) CreateRoom(req models.CreateRoomRequest, creator *models.UserSession) (*models.Room, error) {
	maxUsers, isUnlimited, err := NormalizeCapacity(req.MaxUsers, req.IsUnlimited)
	if err != nil {
		return nil, err
	}

	var passwordHash string
	if req.IsPrivate {
		if req.Password == "" {
			return nil, errors.New("password is required for private rooms")
		}
		hashed, err := HashPassword(req.Password)
		if err != nil {
			return nil, fmt.Errorf("failed to hash password: %w", err)
		}
		passwordHash = hashed
	}

	room := &models.Room{
		ID:           uuid.NewString(),
		Name:         req.Name,
		IsPrivate:    req.IsPrivate,
		PasswordHash: passwordHash,
		MaxUsers:     maxUsers,
		IsUnlimited:  isUnlimited,
		CurrentUsers: 0,
		CreatedBy:    creator.UserID,
		CreatorName:  creator.Nickname,
		CreatedAt:    time.Now().UTC(),
	}

	if err := m.store.CreateRoom(room); err != nil {
		return nil, err
	}

	hub := newRoomHub(room, m.store, m.removeEmptyRoom)
	m.mu.Lock()
	m.hubs[room.ID] = hub
	m.mu.Unlock()

	go hub.Run()
	return room, nil
}

// ListRooms returns all rooms matching the visibility filter ("all", "public", "private")
// populated with real-time CurrentUsers count.
func (m *Manager) ListRooms(visibilityFilter string) ([]*models.Room, error) {
	rooms, err := m.store.ListRooms(visibilityFilter)
	if err != nil {
		return nil, err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, r := range rooms {
		if hub, ok := m.hubs[r.ID]; ok {
			r.CurrentUsers = hub.CurrentUsers()
		}
	}
	return rooms, nil
}

// GetRoom returns a room and its active RoomHub.
func (m *Manager) GetRoom(roomID string) (*models.Room, *RoomHub, error) {
	m.mu.RLock()
	hub, ok := m.hubs[roomID]
	m.mu.RUnlock()
	if !ok {
		return nil, nil, ErrRoomNotFound
	}

	// Copy room struct to avoid data races when setting CurrentUsers
	roomCopy := *hub.room
	roomCopy.CurrentUsers = hub.CurrentUsers()
	return &roomCopy, hub, nil
}

// VerifyAccess checks if a room exists, if the password is valid (for private rooms),
// and whether the room currently has available capacity.
func (m *Manager) VerifyAccess(roomID, password string) (*models.Room, *RoomHub, error) {
	room, hub, err := m.GetRoom(roomID)
	if err != nil {
		return nil, nil, err
	}

	if room.IsPrivate {
		if !VerifyPassword(room.PasswordHash, password) {
			return nil, nil, ErrInvalidPassword
		}
	}

	if hub.CurrentUsers() >= room.MaxUsers {
		return nil, nil, ErrRoomFull
	}

	return room, hub, nil
}

// DeleteRoom removes a room if requested by its creator and disconnects active clients.
func (m *Manager) DeleteRoom(roomID, requesterUserID string) error {
	m.mu.Lock()
	hub, ok := m.hubs[roomID]
	if !ok {
		m.mu.Unlock()
		return ErrRoomNotFound
	}
	if hub.room.CreatedBy != requesterUserID {
		m.mu.Unlock()
		return ErrForbidden
	}
	delete(m.hubs, roomID)
	m.mu.Unlock()

	hub.Stop()
	return m.store.DeleteRoom(roomID)
}
