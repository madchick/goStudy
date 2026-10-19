package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"goChatServer/internal/models"

	_ "modernc.org/sqlite"
)

var (
	ErrNotFound      = errors.New("record not found")
	ErrNicknameTaken = errors.New("nickname is already taken")
	ErrRoomNameTaken = errors.New("room name is already taken")
)

const (
	batchFlushInterval = 500 * time.Millisecond
	maxBatchSize       = 500
	messageQueueSize   = 10000
)

// Store handles database persistence for sessions, rooms, and chat messages.
type Store struct {
	db        *sql.DB
	msgQueue  chan *models.Message
	flushCh   chan chan error
	stopCh    chan struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once
	nextMsgID atomic.Int64
}

// NewStore opens a SQLite database (file path or ":memory:"), initializes tables,
// and starts the background batch writer that flushes queued messages every 0.5s.
func NewStore(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite db: %w", err)
	}

	// Set pragmas for high concurrency and fast batch writes in SQLite
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=NORMAL; PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON;`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to set sqlite pragmas: %w", err)
	}

	s := &Store{
		db:       db,
		msgQueue: make(chan *models.Message, messageQueueSize),
		flushCh:  make(chan chan error),
		stopCh:   make(chan struct{}),
	}
	if err := s.initSchema(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := s.initLastMessageID(); err != nil {
		_ = db.Close()
		return nil, err
	}

	s.wg.Add(1)
	go s.batchWriteLoop()

	return s, nil
}

func (s *Store) initSchema() error {
	schema := `
	CREATE TABLE IF NOT EXISTS user_sessions (
		token TEXT PRIMARY KEY,
		user_id TEXT NOT NULL,
		nickname TEXT NOT NULL UNIQUE COLLATE NOCASE,
		password_hash TEXT NOT NULL DEFAULT '',
		created_at DATETIME NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_user_sessions_user_id ON user_sessions(user_id);

	-- Clean up any legacy duplicate nicknames (keep the newest row) before creating the unique index
	DELETE FROM user_sessions
	WHERE rowid NOT IN (
		SELECT MAX(rowid) FROM user_sessions GROUP BY LOWER(nickname)
	);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_user_sessions_nickname_unique ON user_sessions(nickname COLLATE NOCASE);

	CREATE TABLE IF NOT EXISTS rooms (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL UNIQUE COLLATE NOCASE,
		is_private INTEGER NOT NULL DEFAULT 0,
		password_hash TEXT NOT NULL DEFAULT '',
		max_users INTEGER NOT NULL DEFAULT 100,
		is_unlimited INTEGER NOT NULL DEFAULT 0,
		created_by TEXT NOT NULL,
		creator_name TEXT NOT NULL,
		created_at DATETIME NOT NULL
	);

	-- Clean up any legacy duplicate room names (keep the newest row) before creating the unique index
	DELETE FROM rooms
	WHERE rowid NOT IN (
		SELECT MAX(rowid) FROM rooms GROUP BY LOWER(name)
	);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_rooms_name_unique ON rooms(name COLLATE NOCASE);

	CREATE TABLE IF NOT EXISTS messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		room_id TEXT NOT NULL,
		user_id TEXT NOT NULL,
		nickname TEXT NOT NULL,
		type TEXT NOT NULL,
		content TEXT NOT NULL,
		created_at DATETIME NOT NULL,
		FOREIGN KEY(room_id) REFERENCES rooms(id) ON DELETE CASCADE
	);
	CREATE INDEX IF NOT EXISTS idx_messages_room_id_id ON messages(room_id, id DESC);
	`
	_, err := s.db.Exec(schema)
	if err != nil {
		return fmt.Errorf("failed to initialize schema: %w", err)
	}

	// Ensure password_hash column exists on existing chat.db files created before this column was added
	_, _ = s.db.Exec(`ALTER TABLE user_sessions ADD COLUMN password_hash TEXT NOT NULL DEFAULT ''`)

	// Clear any leftover rooms and messages from previous server runs so room names can be reused after restart
	if err := s.ClearAllRooms(); err != nil {
		return fmt.Errorf("failed to clear leftover rooms on startup: %w", err)
	}

	return nil
}

// ClearAllRooms deletes all rooms and their associated messages from SQLite.
func (s *Store) ClearAllRooms() error {
	if _, err := s.db.Exec(`DELETE FROM messages; DELETE FROM rooms;`); err != nil {
		return err
	}
	return nil
}

func (s *Store) initLastMessageID() error {
	var maxID sql.NullInt64
	if err := s.db.QueryRow(`SELECT MAX(id) FROM messages`).Scan(&maxID); err != nil {
		return fmt.Errorf("failed to query max message id: %w", err)
	}
	if maxID.Valid {
		s.nextMsgID.Store(maxID.Int64)
	}
	return nil
}

// batchWriteLoop collects messages in memory and commits them in a single transaction
// every 500ms (or immediately when batch reaches maxBatchSize / Flush is called).
func (s *Store) batchWriteLoop() {
	defer s.wg.Done()

	ticker := time.NewTicker(batchFlushInterval)
	defer ticker.Stop()

	batch := make([]*models.Message, 0, maxBatchSize)

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := s.bulkInsertMessages(batch)
		if err != nil {
			log.Printf("[Store] bulk insert error (%d messages): %v", len(batch), err)
		}
		clear(batch)
		batch = batch[:0]
		return err
	}

	for {
		select {
		case msg := <-s.msgQueue:
			batch = append(batch, msg)
			if len(batch) >= maxBatchSize {
				_ = flush()
			}

		case <-ticker.C:
			_ = flush()

		case errCh := <-s.flushCh:
			// Drain any currently queued messages into the batch before flushing
		drainLoop:
			for {
				select {
				case msg := <-s.msgQueue:
					batch = append(batch, msg)
					if len(batch) >= maxBatchSize {
						_ = flush()
					}
				default:
					break drainLoop
				}
			}
			errCh <- flush()

		case <-s.stopCh:
			// Drain remaining queued messages before shutting down
		shutdownDrain:
			for {
				select {
				case msg := <-s.msgQueue:
					batch = append(batch, msg)
					if len(batch) >= maxBatchSize {
						_ = flush()
					}
				default:
					break shutdownDrain
				}
			}
			_ = flush()
			return
		}
	}
}

// bulkInsertMessages writes a slice of messages inside a single SQLite transaction.
func (s *Store) bulkInsertMessages(batch []*models.Message) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		_ = tx.Rollback()
	}()

	stmt, err := tx.Prepare(`
		INSERT INTO messages (id, room_id, user_id, nickname, type, content, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, msg := range batch {
		if _, err := stmt.Exec(
			msg.ID,
			msg.RoomID,
			msg.UserID,
			msg.Nickname,
			string(msg.Type),
			msg.Content,
			msg.CreatedAt.UTC(),
		); err != nil {
			// If room was deleted before flush, skip that message so other rooms' messages succeed
			log.Printf("[Store] skipping message id=%d (room=%s): %v", msg.ID, msg.RoomID, err)
			continue
		}
	}

	return tx.Commit()
}

// EnqueueMessage assigns a monotonically increasing ID and queues the message in memory
// to be bulk-inserted on the next 0.5s tick.
func (s *Store) EnqueueMessage(msg *models.Message) {
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now().UTC()
	}
	if msg.ID == 0 {
		msg.ID = s.nextMsgID.Add(1)
	}

	select {
	case s.msgQueue <- msg:
	default:
		// Fallback if queue is completely full under extreme spike
		log.Printf("[Store] message queue full, performing synchronous fallback insert for room=%s", msg.RoomID)
		_ = s.bulkInsertMessages([]*models.Message{msg})
	}
}

// Flush forces an immediate bulk insert of all currently queued messages.
func (s *Store) Flush() error {
	errCh := make(chan error, 1)
	select {
	case s.flushCh <- errCh:
		return <-errCh
	case <-s.stopCh:
		return nil
	}
}

// Close flushes any pending messages, deletes all rooms/messages on server shutdown, and closes the SQLite connection.
func (s *Store) Close() error {
	var closeErr error
	s.closeOnce.Do(func() {
		close(s.stopCh)
		s.wg.Wait()
		_ = s.ClearAllRooms()
		closeErr = s.db.Close()
	})
	return closeErr
}

// SaveOrReclaimSession either registers a new nickname with its passwordHash,
// or if the nickname already exists, verifies the password via verifyFn and rotates the session token
// while preserving the user's original UserID.
// Returns (session, isReclaimed, error).
func (s *Store) SaveOrReclaimSession(sess *models.UserSession, passwordHash string, rawPassword string, verifyFn func(storedHash, plain string) bool) (*models.UserSession, bool, error) {
	var existingUserID, existingNickname, existingHash string
	var existingCreatedAt time.Time
	err := s.db.QueryRow(
		`SELECT user_id, nickname, password_hash, created_at FROM user_sessions WHERE nickname = ? COLLATE NOCASE LIMIT 1`,
		sess.Nickname,
	).Scan(&existingUserID, &existingNickname, &existingHash, &existingCreatedAt)

	if err == nil {
		// Nickname already exists: verify password to reclaim
		if existingHash == "" || rawPassword == "" || !verifyFn(existingHash, rawPassword) {
			return nil, false, ErrNicknameTaken
		}
		// Password matches! Rotate token and keep the original user_id & canonical nickname
		_, err := s.db.Exec(
			`UPDATE user_sessions SET token = ? WHERE user_id = ?`,
			sess.Token, existingUserID,
		)
		if err != nil {
			return nil, false, err
		}
		return &models.UserSession{
			UserID:    existingUserID,
			Nickname:  existingNickname,
			Token:     sess.Token,
			CreatedAt: existingCreatedAt,
		}, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}

	_, err = s.db.Exec(
		`INSERT INTO user_sessions (token, user_id, nickname, password_hash, created_at) VALUES (?, ?, ?, ?, ?)`,
		sess.Token, sess.UserID, sess.Nickname, passwordHash, sess.CreatedAt.UTC(),
	)
	if err != nil && isUniqueConstraintError(err) {
		return nil, false, ErrNicknameTaken
	}
	if err != nil {
		return nil, false, err
	}
	return sess, false, nil
}

// GetSessionByToken retrieves a user session by its bearer token.
func (s *Store) GetSessionByToken(token string) (*models.UserSession, error) {
	if token == "" {
		return nil, ErrNotFound
	}
	var sess models.UserSession
	err := s.db.QueryRow(
		`SELECT token, user_id, nickname, created_at FROM user_sessions WHERE token = ?`,
		token,
	).Scan(&sess.Token, &sess.UserID, &sess.Nickname, &sess.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &sess, nil
}

// UpdateSessionNickname updates the nickname for an existing session token, returning ErrNicknameTaken if another user already uses it.
func (s *Store) UpdateSessionNickname(token, nickname string) error {
	var existingToken string
	err := s.db.QueryRow(`SELECT token FROM user_sessions WHERE nickname = ? COLLATE NOCASE LIMIT 1`, nickname).Scan(&existingToken)
	if err == nil && existingToken != token {
		return ErrNicknameTaken
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	res, err := s.db.Exec(`UPDATE user_sessions SET nickname = ? WHERE token = ?`, nickname, token)
	if err != nil {
		if isUniqueConstraintError(err) {
			return ErrNicknameTaken
		}
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

func isUniqueConstraintError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") || strings.Contains(msg, "constraint failed")
}

// CreateRoom persists a new chat room, returning ErrRoomNameTaken if a room with the same name already exists.
func (s *Store) CreateRoom(room *models.Room) error {
	var exists int
	err := s.db.QueryRow(`SELECT 1 FROM rooms WHERE name = ? COLLATE NOCASE LIMIT 1`, room.Name).Scan(&exists)
	if err == nil {
		return ErrRoomNameTaken
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	isPrivate := 0
	if room.IsPrivate {
		isPrivate = 1
	}
	isUnlimited := 0
	if room.IsUnlimited {
		isUnlimited = 1
	}
	_, err = s.db.Exec(
		`INSERT INTO rooms (id, name, is_private, password_hash, max_users, is_unlimited, created_by, creator_name, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		room.ID, room.Name, isPrivate, room.PasswordHash, room.MaxUsers, isUnlimited, room.CreatedBy, room.CreatorName, room.CreatedAt.UTC(),
	)
	if err != nil && isUniqueConstraintError(err) {
		return ErrRoomNameTaken
	}
	return err
}

// GetRoom retrieves a chat room by ID.
func (s *Store) GetRoom(id string) (*models.Room, error) {
	var room models.Room
	var isPrivate, isUnlimited int
	err := s.db.QueryRow(
		`SELECT id, name, is_private, password_hash, max_users, is_unlimited, created_by, creator_name, created_at
		 FROM rooms WHERE id = ?`,
		id,
	).Scan(
		&room.ID, &room.Name, &isPrivate, &room.PasswordHash,
		&room.MaxUsers, &isUnlimited, &room.CreatedBy, &room.CreatorName, &room.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	room.IsPrivate = isPrivate == 1
	room.IsUnlimited = isUnlimited == 1
	return &room, nil
}

// ListRooms retrieves all rooms, optionally filtered by visibility ("all", "public", "private").
func (s *Store) ListRooms(visibilityFilter string) ([]*models.Room, error) {
	query := `SELECT id, name, is_private, password_hash, max_users, is_unlimited, created_by, creator_name, created_at FROM rooms`
	var args []any

	switch visibilityFilter {
	case "public":
		query += ` WHERE is_private = 0`
	case "private":
		query += ` WHERE is_private = 1`
	}
	query += ` ORDER BY created_at DESC`

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	rooms := make([]*models.Room, 0)
	for rows.Next() {
		var r models.Room
		var isPrivate, isUnlimited int
		if err := rows.Scan(
			&r.ID, &r.Name, &isPrivate, &r.PasswordHash,
			&r.MaxUsers, &isUnlimited, &r.CreatedBy, &r.CreatorName, &r.CreatedAt,
		); err != nil {
			return nil, err
		}
		r.IsPrivate = isPrivate == 1
		r.IsUnlimited = isUnlimited == 1
		rooms = append(rooms, &r)
	}
	return rooms, rows.Err()
}

// DeleteRoom deletes a room by ID after flushing pending messages.
func (s *Store) DeleteRoom(id string) error {
	_ = s.Flush()
	res, err := s.db.Exec(`DELETE FROM rooms WHERE id = ?`, id)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// GetRecentMessages returns up to `limit` messages for a room in chronological order (oldest to newest among the recent slice).
// If beforeID > 0, it fetches messages with id < beforeID for cursor-based pagination.
func (s *Store) GetRecentMessages(roomID string, limit int, beforeID int64) ([]models.Message, error) {
	// Ensure any queued messages within the current 0.5s window are flushed before reading history
	_ = s.Flush()

	if limit <= 0 || limit > 200 {
		limit = 50
	}

	var rows *sql.Rows
	var err error
	if beforeID > 0 {
		rows, err = s.db.Query(
			`SELECT id, room_id, user_id, nickname, type, content, created_at
			 FROM messages
			 WHERE room_id = ? AND id < ?
			 ORDER BY id DESC
			 LIMIT ?`,
			roomID, beforeID, limit,
		)
	} else {
		rows, err = s.db.Query(
			`SELECT id, room_id, user_id, nickname, type, content, created_at
			 FROM messages
			 WHERE room_id = ?
			 ORDER BY id DESC
			 LIMIT ?`,
			roomID, limit,
		)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var descMsgs []models.Message
	for rows.Next() {
		var m models.Message
		var msgType string
		if err := rows.Scan(&m.ID, &m.RoomID, &m.UserID, &m.Nickname, &msgType, &m.Content, &m.CreatedAt); err != nil {
			return nil, err
		}
		m.Type = models.MessageType(msgType)
		descMsgs = append(descMsgs, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Reverse to chronological order (oldest -> newest)
	msgs := make([]models.Message, len(descMsgs))
	for i := range descMsgs {
		msgs[i] = descMsgs[len(descMsgs)-1-i]
	}
	return msgs, nil
}
