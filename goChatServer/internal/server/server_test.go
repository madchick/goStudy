package server_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"goChatServer/internal/chat"
	"goChatServer/internal/models"
	"goChatServer/internal/server"
	"goChatServer/internal/storage"

	"github.com/gorilla/websocket"
)

func setupTestServer(t *testing.T) (*httptest.Server, *storage.Store) {
	t.Helper()
	store, err := storage.NewStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create in-memory store: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})

	manager, err := chat.NewManager(store)
	if err != nil {
		t.Fatalf("failed to create chat manager: %v", err)
	}

	srv := server.NewServer(store, manager)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts, store
}

func createSession(t *testing.T, baseURL, nickname string) models.UserSession {
	t.Helper()
	body, _ := json.Marshal(models.CreateSessionRequest{Nickname: nickname})
	resp, err := http.Post(baseURL+"/api/auth/session", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("failed to create session: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d", resp.StatusCode)
	}

	var sess models.UserSession
	if err := json.NewDecoder(resp.Body).Decode(&sess); err != nil {
		t.Fatalf("failed to decode session: %v", err)
	}
	return sess
}

func createRoom(t *testing.T, baseURL, token string, req models.CreateRoomRequest) (*http.Response, *models.Room) {
	t.Helper()
	body, _ := json.Marshal(req)
	httpReq, _ := http.NewRequest(http.MethodPost, baseURL+"/api/rooms", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("createRoom request failed: %v", err)
	}

	if resp.StatusCode != http.StatusCreated {
		return resp, nil
	}
	defer resp.Body.Close()

	var room models.Room
	if err := json.NewDecoder(resp.Body).Decode(&room); err != nil {
		t.Fatalf("failed to decode room: %v", err)
	}
	return resp, &room
}

func TestRoomCreationAndCapacityRules(t *testing.T) {
	ts, _ := setupTestServer(t)
	alice := createSession(t, ts.URL, "Alice")

	// 1. Unlimited room -> should set MaxUsers = 100 and IsUnlimited = true
	_, unlimitedRoom := createRoom(t, ts.URL, alice.Token, models.CreateRoomRequest{
		Name:        "오픈 채팅방 (무제한)",
		IsPrivate:   false,
		IsUnlimited: true,
	})
	if unlimitedRoom.MaxUsers != 100 || !unlimitedRoom.IsUnlimited {
		t.Fatalf("expected unlimited room to have MaxUsers=100 & IsUnlimited=true, got MaxUsers=%d, IsUnlimited=%v",
			unlimitedRoom.MaxUsers, unlimitedRoom.IsUnlimited)
	}

	// 2. Custom capacity room (e.g., 2 users)
	_, smallRoom := createRoom(t, ts.URL, alice.Token, models.CreateRoomRequest{
		Name:      "소규모 방",
		IsPrivate: false,
		MaxUsers:  2,
	})
	if smallRoom.MaxUsers != 2 || smallRoom.IsUnlimited {
		t.Fatalf("expected MaxUsers=2 & IsUnlimited=false, got MaxUsers=%d, IsUnlimited=%v",
			smallRoom.MaxUsers, smallRoom.IsUnlimited)
	}

	// 3. Exceeding 100 users -> 400 Bad Request
	resp, _ := createRoom(t, ts.URL, alice.Token, models.CreateRoomRequest{
		Name:     "초과 방",
		MaxUsers: 101,
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for MaxUsers=101, got %d", resp.StatusCode)
	}

	// 4. Private room shows in list, requires password to verify/join
	_, privRoom := createRoom(t, ts.URL, alice.Token, models.CreateRoomRequest{
		Name:      "비공개 스터디방",
		IsPrivate: true,
		Password:  "secret1234",
		MaxUsers:  10,
	})
	if !privRoom.IsPrivate {
		t.Fatalf("expected private room IsPrivate=true")
	}

	// List all rooms -> should include both public and private rooms
	listResp, err := http.Get(ts.URL + "/api/rooms")
	if err != nil {
		t.Fatalf("failed to list rooms: %v", err)
	}
	defer listResp.Body.Close()

	var listData struct {
		Rooms []models.Room `json:"rooms"`
		Total int           `json:"total"`
	}
	_ = json.NewDecoder(listResp.Body).Decode(&listData)
	if listData.Total != 3 {
		t.Fatalf("expected 3 rooms in list (both public and private), got %d", listData.Total)
	}
}

func TestWebSocketChatPrivatePasswordAndCapacityEnforcement(t *testing.T) {
	ts, _ := setupTestServer(t)
	alice := createSession(t, ts.URL, "Alice")
	bob := createSession(t, ts.URL, "Bob")
	charlie := createSession(t, ts.URL, "Charlie")

	// Create a private room with MaxUsers = 2
	_, privRoom := createRoom(t, ts.URL, alice.Token, models.CreateRoomRequest{
		Name:      "비공개 2인방",
		IsPrivate: true,
		Password:  "pass9999",
		MaxUsers:  2,
	})

	wsBase := "ws" + strings.TrimPrefix(ts.URL, "http")

	// 1. Try joining private room with wrong password -> should fail with 403 Forbidden
	_, badResp, err := websocket.DefaultDialer.Dial(
		wsBase+"/ws/rooms/"+privRoom.ID+"?token="+alice.Token+"&password=wrong",
		nil,
	)
	if err == nil {
		t.Fatalf("expected error dialing private room with wrong password")
	}
	if badResp == nil || badResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for wrong password, got %v", badResp)
	}

	// 2. Alice joins with correct password
	aliceConn, _, err := websocket.DefaultDialer.Dial(
		wsBase+"/ws/rooms/"+privRoom.ID+"?token="+alice.Token+"&password=pass9999",
		nil,
	)
	if err != nil {
		t.Fatalf("Alice failed to join private room: %v", err)
	}
	defer aliceConn.Close()

	// 3. Bob joins with correct password via Header
	bobHeaders := http.Header{}
	bobHeaders.Set("Authorization", "Bearer "+bob.Token)
	bobHeaders.Set("X-Room-Password", "pass9999")
	bobConn, _, err := websocket.DefaultDialer.Dial(
		wsBase+"/ws/rooms/"+privRoom.ID,
		bobHeaders,
	)
	if err != nil {
		t.Fatalf("Bob failed to join private room: %v", err)
	}
	defer bobConn.Close()

	// 4. Charlie tries to join when room is full (2/2) -> should fail with 409 Conflict
	_, fullResp, err := websocket.DefaultDialer.Dial(
		wsBase+"/ws/rooms/"+privRoom.ID+"?token="+charlie.Token+"&password=pass9999",
		nil,
	)
	if err == nil {
		t.Fatalf("expected error dialing full room")
	}
	if fullResp == nil || fullResp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict for full room, got %v", fullResp)
	}

	// 5. Alice sends a chat message and Bob receives it
	chatPayload, _ := json.Marshal(models.IncomingWSMessage{
		Type:    models.MessageTypeChat,
		Content: "안녕하세요 Bob!",
	})
	if err := aliceConn.WriteMessage(websocket.TextMessage, chatPayload); err != nil {
		t.Fatalf("Alice failed to send message: %v", err)
	}

	_ = bobConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	foundChat := false
	for i := 0; i < 6; i++ {
		_, raw, err := bobConn.ReadMessage()
		if err != nil {
			t.Fatalf("Bob failed reading message: %v", err)
		}
		var msg models.Message
		if err := json.Unmarshal(raw, &msg); err == nil && msg.Type == models.MessageTypeChat {
			if msg.Content == "안녕하세요 Bob!" && msg.Nickname == "Alice" {
				foundChat = true
				break
			}
		}
	}
	if !foundChat {
		t.Fatalf("Bob did not receive Alice's chat message")
	}

	// 6. Verify message history is persisted in SQLite via GET /api/rooms/{id}/messages
	histReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/rooms/"+privRoom.ID+"/messages", nil)
	histReq.Header.Set("Authorization", "Bearer "+alice.Token)
	histReq.Header.Set("X-Room-Password", "pass9999")
	histResp, err := http.DefaultClient.Do(histReq)
	if err != nil {
		t.Fatalf("failed to fetch message history: %v", err)
	}
	defer histResp.Body.Close()

	if histResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from messages endpoint, got %d", histResp.StatusCode)
	}

	var histData struct {
		Messages []models.Message `json:"messages"`
	}
	_ = json.NewDecoder(histResp.Body).Decode(&histData)

	hasPersistedChat := false
	for _, m := range histData.Messages {
		if m.Type == models.MessageTypeChat && m.Content == "안녕하세요 Bob!" {
			hasPersistedChat = true
			break
		}
	}
	if !hasPersistedChat {
		t.Fatalf("expected chat message to be persisted in SQLite history, got %+v", histData.Messages)
	}
}

func TestBatchBulkInsertAndSHA256Salt(t *testing.T) {
	// 1. Verify SHA-256 + Salt hashing and constant-time verification
	hash, err := chat.HashPassword("my-secret-pw")
	if err != nil {
		t.Fatalf("HashPassword failed: %v", err)
	}
	if !strings.Contains(hash, "$") {
		t.Fatalf("expected salt$hash format, got %s", hash)
	}
	if !chat.VerifyPassword(hash, "my-secret-pw") {
		t.Fatalf("expected VerifyPassword to succeed for correct password")
	}
	if chat.VerifyPassword(hash, "wrong-pw") {
		t.Fatalf("expected VerifyPassword to fail for wrong password")
	}

	// 2. Verify 0.5s batch bulk insert into SQLite
	ts, store := setupTestServer(t)
	alice := createSession(t, ts.URL, "Alice")
	_, pubRoom := createRoom(t, ts.URL, alice.Token, models.CreateRoomRequest{
		Name:        "배치 테스트방",
		IsPrivate:   false,
		IsUnlimited: true,
	})

	// Enqueue 50 messages in memory
	for i := 0; i < 50; i++ {
		store.EnqueueMessage(&models.Message{
			RoomID:    pubRoom.ID,
			UserID:    alice.UserID,
			Nickname:  alice.Nickname,
			Type:      models.MessageTypeChat,
			Content:   "배치 메시지 테스트",
			CreatedAt: time.Now().UTC(),
		})
	}

	// Wait slightly over 0.5s for the periodic ticker to bulk-insert the transaction
	time.Sleep(650 * time.Millisecond)

	msgs, err := store.GetRecentMessages(pubRoom.ID, 100, 0)
	if err != nil {
		t.Fatalf("GetRecentMessages failed: %v", err)
	}
	if len(msgs) != 50 {
		t.Fatalf("expected 50 bulk-inserted messages after 0.5s tick, got %d", len(msgs))
	}
}

func TestUniqueNicknameAndAutoDeleteEmptyRoom(t *testing.T) {
	ts, _ := setupTestServer(t)

	// 1. Create session with nickname "Alice" and password "alice-pin-1234"
	aliceInitBody, _ := json.Marshal(models.CreateSessionRequest{
		Nickname: "Alice",
		Password: "alice-pin-1234",
	})
	aliceInitResp, err := http.Post(ts.URL+"/api/auth/session", "application/json", bytes.NewReader(aliceInitBody))
	if err != nil {
		t.Fatalf("failed to create Alice session: %v", err)
	}
	if aliceInitResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created for new Alice session, got %d", aliceInitResp.StatusCode)
	}
	var alice models.UserSession
	_ = json.NewDecoder(aliceInitResp.Body).Decode(&alice)
	aliceInitResp.Body.Close()

	// 2. Try creating another session with the same nickname "Alice" without password or wrong password -> 409 Conflict
	dupBody, _ := json.Marshal(models.CreateSessionRequest{Nickname: "Alice", Password: "wrong-password"})
	dupResp, err := http.Post(ts.URL+"/api/auth/session", "application/json", bytes.NewReader(dupBody))
	if err != nil {
		t.Fatalf("failed request: %v", err)
	}
	dupResp.Body.Close()
	if dupResp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict for duplicate nickname with wrong password, got %d", dupResp.StatusCode)
	}

	// 2-b. Reclaim "Alice" with the correct password -> 200 OK, preserves UserID, issues new Token
	reclaimResp, err := http.Post(ts.URL+"/api/auth/session", "application/json", bytes.NewReader(aliceInitBody))
	if err != nil {
		t.Fatalf("failed reclaim request: %v", err)
	}
	if reclaimResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK when reclaiming nickname with correct password, got %d", reclaimResp.StatusCode)
	}
	var reclaimedAlice models.UserSession
	_ = json.NewDecoder(reclaimResp.Body).Decode(&reclaimedAlice)
	reclaimResp.Body.Close()
	if reclaimedAlice.UserID != alice.UserID {
		t.Fatalf("expected reclaimed session to preserve UserID %s, got %s", alice.UserID, reclaimedAlice.UserID)
	}
	alice = reclaimedAlice

	// 3. Create "Bob" and try updating Bob's nickname to "Alice" -> 409 Conflict
	bob := createSession(t, ts.URL, "Bob")
	updReq, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/auth/nickname", bytes.NewReader(dupBody))
	updReq.Header.Set("Content-Type", "application/json")
	updReq.Header.Set("Authorization", "Bearer "+bob.Token)
	updResp, err := http.DefaultClient.Do(updReq)
	if err != nil {
		t.Fatalf("failed update request: %v", err)
	}
	updResp.Body.Close()
	if updResp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict when changing nickname to existing one, got %d", updResp.StatusCode)
	}

	// 4. Create a room, verify duplicate room name is rejected with 409 Conflict,
	// then have Alice and Bob join and both disconnect -> room should be automatically deleted
	_, room := createRoom(t, ts.URL, alice.Token, models.CreateRoomRequest{
		Name:        "자동 폭파 테스트방",
		IsPrivate:   false,
		IsUnlimited: true,
	})

	dupRoomResp, _ := createRoom(t, ts.URL, bob.Token, models.CreateRoomRequest{
		Name:        "자동 폭파 테스트방",
		IsPrivate:   false,
		IsUnlimited: true,
	})
	dupRoomResp.Body.Close()
	if dupRoomResp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict for duplicate room name, got %d", dupRoomResp.StatusCode)
	}

	wsBase := "ws" + strings.TrimPrefix(ts.URL, "http")
	aliceConn, _, err := websocket.DefaultDialer.Dial(wsBase+"/ws/rooms/"+room.ID+"?token="+alice.Token, nil)
	if err != nil {
		t.Fatalf("Alice failed to join: %v", err)
	}
	bobConn, _, err := websocket.DefaultDialer.Dial(wsBase+"/ws/rooms/"+room.ID+"?token="+bob.Token, nil)
	if err != nil {
		t.Fatalf("Bob failed to join: %v", err)
	}

	// Alice disconnects first -> Bob is still in the room, so room must still exist
	_ = aliceConn.Close()
	time.Sleep(50 * time.Millisecond)

	checkResp1, err := http.Get(ts.URL + "/api/rooms/" + room.ID)
	if err != nil {
		t.Fatalf("failed to get room: %v", err)
	}
	checkResp1.Body.Close()
	if checkResp1.StatusCode != http.StatusOK {
		t.Fatalf("expected room to still exist while Bob is connected, got %d", checkResp1.StatusCode)
	}

	// Bob (the last person) disconnects -> room must be automatically deleted
	_ = bobConn.Close()

	deleted := false
	for i := 0; i < 20; i++ {
		time.Sleep(25 * time.Millisecond)
		checkResp2, err := http.Get(ts.URL + "/api/rooms/" + room.ID)
		if err != nil {
			t.Fatalf("failed to get room: %v", err)
		}
		checkResp2.Body.Close()
		if checkResp2.StatusCode == http.StatusNotFound {
			deleted = true
			break
		}
	}
	if !deleted {
		t.Fatalf("expected room to be automatically deleted after the last user disconnected")
	}

	// 5. Once the room is auto-deleted, creating a new room with the same name should succeed again
	recreateResp, recreatedRoom := createRoom(t, ts.URL, bob.Token, models.CreateRoomRequest{
		Name:        "자동 폭파 테스트방",
		IsPrivate:   false,
		IsUnlimited: true,
	})
	if recreateResp.StatusCode != http.StatusCreated || recreatedRoom == nil {
		t.Fatalf("expected 201 Created when reusing room name after previous room was deleted, got %d", recreateResp.StatusCode)
	}
}

func TestServerRestartClearsRoomsButKeepsUserSessions(t *testing.T) {
	dbFile := t.TempDir() + "/restart_test.db"

	// First server run
	store1, err := storage.NewStore(dbFile)
	if err != nil {
		t.Fatalf("failed to open store1: %v", err)
	}
	mgr1, err := chat.NewManager(store1)
	if err != nil {
		t.Fatalf("failed to create mgr1: %v", err)
	}
	srv1 := httptest.NewServer(server.NewServer(store1, mgr1))

	// Register Alice with password and create a room "재기동 테스트방"
	aliceReq, _ := json.Marshal(models.CreateSessionRequest{Nickname: "Alice", Password: "pw123"})
	resp, err := http.Post(srv1.URL+"/api/auth/session", "application/json", bytes.NewReader(aliceReq))
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("failed to create session on run 1: %v", err)
	}
	var alice models.UserSession
	_ = json.NewDecoder(resp.Body).Decode(&alice)
	resp.Body.Close()

	_, room1 := createRoom(t, srv1.URL, alice.Token, models.CreateRoomRequest{
		Name:        "재기동 테스트방",
		IsPrivate:   false,
		IsUnlimited: true,
	})
	if room1 == nil {
		t.Fatalf("expected room1 to be created")
	}

	// Simulate server shutdown
	mgr1.Shutdown()
	srv1.Close()
	_ = store1.Close()

	// Second server run (restart with same DB file)
	store2, err := storage.NewStore(dbFile)
	if err != nil {
		t.Fatalf("failed to open store2: %v", err)
	}
	defer store2.Close()
	mgr2, err := chat.NewManager(store2)
	if err != nil {
		t.Fatalf("failed to create mgr2: %v", err)
	}
	srv2 := httptest.NewServer(server.NewServer(store2, mgr2))
	defer srv2.Close()

	// 1. Room list should be empty after restart
	listResp, err := http.Get(srv2.URL + "/api/rooms")
	if err != nil {
		t.Fatalf("failed to list rooms on run 2: %v", err)
	}
	var listData struct {
		Total int `json:"total"`
	}
	_ = json.NewDecoder(listResp.Body).Decode(&listData)
	listResp.Body.Close()
	if listData.Total != 0 {
		t.Fatalf("expected 0 rooms after server restart, got %d", listData.Total)
	}

	// 2. Creating a room with the exact same name "재기동 테스트방" should succeed
	recreateResp, room2 := createRoom(t, srv2.URL, alice.Token, models.CreateRoomRequest{
		Name:        "재기동 테스트방",
		IsPrivate:   false,
		IsUnlimited: true,
	})
	if recreateResp.StatusCode != http.StatusCreated || room2 == nil {
		t.Fatalf("expected 201 Created for room name reuse after server restart, got %d", recreateResp.StatusCode)
	}
}



