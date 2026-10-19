# Go 멀티플랫폼 실시간 채팅 서버 (`goChatServer`)

웹(Web), 모바일 앱(iOS/Android/Flutter/React Native), 데스크탑 앱(Electron/Tauri/Windows/macOS)에서 공통으로 접속하여 사용할 수 있는 **REST API + WebSocket 기반 Go 채팅 서버**입니다.

---

## 주요 기능

- **멀티플랫폼 호환 (REST + WebSocket + CORS)**
  - 웹 브라우저의 `WebSocket` API(커스텀 헤더 제한 환경)를 위해 **Query Parameter (`?token=...&password=...`)** 방식과 모바일/데스크탑 네이티브 클라이언트를 위한 **HTTP Header (`Authorization: Bearer <token>`, `X-Room-Password`)** 방식을 모두 지원합니다.
- **닉네임 + 비밀번호(PIN) 기반 세션/토큰 발급 및 본인 닉네임 이어쓰기**
  - 동일한 닉네임은 다른 사람이 중복 생성할 수 없습니다 (`UNIQUE COLLATE NOCASE`, `409 Conflict`).
  - 최초 닉네임 생성 시 `password`(비밀번호/PIN)를 함께 지정하면, 나중에 다른 기기(웹/모바일/데스크탑)에서 접속하거나 토큰이 만료/초기화되었을 때도 **같은 닉네임과 비밀번호를 입력해 기존 `user_id`와 닉네임을 그대로 이어서 사용**할 수 있습니다.
- **SQLite 영구 저장 + 0.5초 주기 메모리 큐 Bulk Insert (`modernc.org/sqlite`)**
  - CGO 없이 순수 Go로 동작하는 SQLite(`chat.db`)를 내장하며, 채팅 메시지를 메모리 큐에 모았다가 **0.5초마다 단일 트랜잭션으로 묶어서 Bulk Insert**하여 디스크 I/O 부하를 최소화합니다.
- **비공개방 비밀번호 경량 해싱 (`SHA-256 + 16바이트 난수 Salt`)**
  - 사용자 비밀번호와 비공개 채팅방 비밀번호 모두 `SHA-256 + Salt` 및 상수 시간 비교(`subtle.ConstantTimeCompare`)를 사용하여 작은 인스턴스에서도 CPU 점유율을 낮게 유지합니다.
- **공개 / 비공개 채팅방 목록 조회, 중복 방 이름 금지, 빈 방 및 서버 재기동 시 방 초기화**
  - 모든 채팅방(공개/비공개)을 목록에서 볼 수 있으며(`is_private` 플래그 제공), 서버가 실행 중인 동안 동일한 이름의 채팅방은 중복 생성할 수 없습니다 (`409 Conflict`).
  - 방에 있던 **마지막 사람이 나가거나 연결이 끊기면(`current_users == 0`) 해당 채팅방은 즉시 자동 삭제**됩니다.
  - **서버 종료 또는 재기동 시 모든 채팅방 및 채팅 내역은 일괄 삭제(초기화)**되므로, 재기동 후에는 이전 채팅방 이름을 다시 자유롭게 사용할 수 있습니다. (단, 사용자 닉네임과 비밀번호 세션 정보는 유지됩니다.)
- **채팅방 인원수 제한 (최대 100명)**
  - 방 생성 시 원하는 인원수(`1 ~ 100명`)를 직접 지정하거나, **무제한(`is_unlimited: true` 또는 `max_users: 0`)** 선택 시 자동으로 최대 **100명**으로 설정됩니다.

---

## 실행 및 빌드 방법

```powershell
# 서버 바로 실행 (기본 포트 :8080, DB 파일: chat.db)
go run .

# 실행 파일(.exe) 빌드 후 실행
go build -o goChatServer.exe .
.\goChatServer.exe

# 포트 및 DB 경로 지정 실행
.\goChatServer.exe -addr ":9000" -db "mychat.db"

# 통합 테스트 실행
go test -v ./...
```

---

## 클라이언트 연동 API 명세

### 1. 사용자 인증 및 닉네임 관리

#### `POST /api/auth/session` — 닉네임 등록 또는 기존 닉네임 재사용(로그인)
클라이언트가 최초 접속하거나 재접속할 때 호출합니다.
- **동작 방식:**
  1. **신규 닉네임인 경우 (`201 Created`)**: 새 `user_id`와 세션 `token`을 발급하고 `password`(선택이지만 재사용을 위해 권장)를 해싱하여 저장합니다.
  2. **이미 존재하는 닉네임인 경우**:
     - 요청한 `password`가 기존에 등록된 비밀번호와 **일치하면 (`200 OK`)**: 본인으로 인증하여 **기존 `user_id`와 `nickname`을 그대로 유지**하고 새로운 세션 `token`을 발급해 반환합니다.
     - 비밀번호가 틀리거나 비밀번호 없이 생성된 닉네임이면 **`409 Conflict`** (`{"error": "nickname is already taken or password is incorrect"}`)를 반환합니다.

- **Request:**
```json
{
  "nickname": "홍길동",
  "password": "my-pin-1234"
}
```
- **Response (`201 Created` - 신규 생성 / `200 OK` - 기존 닉네임 재사용 성공):**
```json
{
  "user_id": "550e8400-e29b-41d4-a716-446655440000",
  "nickname": "홍길동",
  "token": "6f8a2b...",
  "created_at": "2026-10-19T09:20:00Z"
}
```
- **Error Responses:**
  - `400 Bad Request`: 닉네임 누락 또는 30자 초과
  - `409 Conflict`: 이미 다른 사용자가 사용 중인 닉네임이거나 비밀번호가 일치하지 않음

> **클라이언트 구현 팁:**
> 발급받은 `token`을 기기 저장소(`localStorage`, `SecureStorage` 등)에 저장해 두고 앱 실행 시 `GET /api/auth/me`로 유효성을 확인하세요. 만약 다른 기기에서 접속했거나 저장된 토큰이 없을 때만 사용자에게 닉네임과 비밀번호를 입력받아 `POST /api/auth/session`을 호출하면 됩니다.

---

#### `GET /api/auth/me` — 저장된 토큰으로 내 세션 확인
- **Header:** `Authorization: Bearer <token>` (또는 `?token=<token>`)
- **Response (`200 OK`):**
```json
{
  "user_id": "550e8400-e29b-41d4-a716-446655440000",
  "nickname": "홍길동",
  "token": "6f8a2b...",
  "created_at": "2026-10-19T09:20:00Z"
}
```

#### `PUT /api/auth/nickname` — 닉네임 변경
- **Header:** `Authorization: Bearer <token>`
- **Request:**
```json
{
  "nickname": "새닉네임"
}
```
- **Response:**
  - `200 OK`: 변경된 세션 정보 반환
  - `409 Conflict`: 이미 사용 중인 닉네임 (`{"error": "nickname is already taken"}`)

---

### 2. 채팅방 목록 조회, 생성 및 관리

#### `GET /api/rooms?visibility=all` — 채팅방 리스트 조회
- `visibility` 쿼리 파라미터:
  - `all` (기본값): 공개방 + 비공개방 전체 조회
  - `public`: 공개방만 조회
  - `private`: 비공개방만 조회
- **Response (`200 OK`):**
```json
{
  "rooms": [
    {
      "id": "c41b1e28-...",
      "name": "자유 수다방",
      "is_private": false,
      "max_users": 100,
      "is_unlimited": true,
      "current_users": 5,
      "created_by": "550e8400-...",
      "creator_name": "홍길동",
      "created_at": "2026-10-19T09:21:00Z"
    },
    {
      "id": "a92f3c11-...",
      "name": "비밀 프로젝트방",
      "is_private": true,
      "max_users": 10,
      "is_unlimited": false,
      "current_users": 2,
      "created_by": "550e8400-...",
      "creator_name": "홍길동",
      "created_at": "2026-10-19T09:22:00Z"
    }
  ],
  "total": 2
}
```

#### `POST /api/rooms` — 채팅방 생성
- **Header:** `Authorization: Bearer <token>`
- **규칙:**
  - 이미 존재하는 채팅방 이름(대소문자 무관)으로는 생성할 수 없으며 `409 Conflict`가 반환됩니다.
  - `is_unlimited: true` 또는 `max_users: 0`이면 최대 인원(`max_users`)이 **100명**으로 설정됩니다.
  - 직접 인원을 지정할 경우 `1 ~ 100` 사이여야 합니다.
- **Request (인원 지정 비공개방 예시):**
```json
{
  "name": "비밀 프로젝트방",
  "is_private": true,
  "password": "roomPassword123",
  "max_users": 10,
  "is_unlimited": false
}
```
- **Request (무제한 공개방 예시 — 최대 100명으로 설정됨):**
```json
{
  "name": "오픈 채팅방",
  "is_private": false,
  "is_unlimited": true
}
```
- **Error Responses:**
  - `400 Bad Request`: 방 이름 누락, 비공개방 비밀번호 누락, 또는 `max_users`가 1~100 범위를 벗어남
  - `409 Conflict`: 동일한 이름의 채팅방이 이미 존재함 (`{"error": "room name is already taken"}`)

#### `GET /api/rooms/{id}` — 특정 채팅방 정보 및 실시간 참여자 목록 조회
- **Response (`200 OK`):**
```json
{
  "room": {
    "id": "c41b1e28-...",
    "name": "오픈 채팅방",
    "is_private": false,
    "max_users": 100,
    "is_unlimited": true,
    "current_users": 2,
    "created_by": "550e8400-...",
    "creator_name": "홍길동",
    "created_at": "2026-10-19T09:21:00Z"
  },
  "participants": [
    { "user_id": "550e8400-...", "nickname": "홍길동" },
    { "user_id": "771a9300-...", "nickname": "이순신" }
  ]
}
```

#### `POST /api/rooms/{id}/verify` — 입장 전 비밀번호/정원 사전 확인 (선택 사항)
클라이언트 UI에서 비공개 방 클릭 시 WebSocket 연결 전에 비밀번호와 정원을 먼저 검증할 때 사용합니다.
- **Request:**
```json
{
  "password": "roomPassword123"
}
```
- **Response:**
  - `200 OK`: 입장 가능 (`{"allowed": true, "room": {...}}`)
  - `403 Forbidden`: 비밀번호 불일치 (`{"error": "invalid room password"}`)
  - `404 Not Found`: 존재하지 않거나 이미 삭제된 방
  - `409 Conflict`: 방 정원 초과 (`current_users >= max_users`)

#### `GET /api/rooms/{id}/messages?limit=50&before_id=0` — 이전 대화 내역 조회
- **Header:** `Authorization: Bearer <token>` (비공개방인 경우 `X-Room-Password: <password>` 헤더 또는 `?password=<password>` 쿼리 추가)
- `before_id`: 무한 스크롤(이전 메시지 더 불러오기) 시 가장 오래된 메시지 `id`를 전달하면 그 이전 메시지들을 반환합니다.

---

### 3. 실시간 WebSocket 접속 및 채팅

#### 접속 URL
```
ws://<서버주소>:8080/ws/rooms/{room_id}?token=<발급받은_토큰>&password=<비공개방_비밀번호>
```
> - 공개방인 경우 `password` 파라미터는 생략합니다.
> - 모바일/데스크탑 앱에서는 쿼리 파라미터 대신 `Authorization: Bearer <token>` 및 `X-Room-Password: <password>` 헤더로도 연결할 수 있습니다.
> - **채팅방 자동 삭제 규칙:** 방에 접속한 사용자들이 나가거나 연결이 끊겨서 **방의 현재 인원이 0명이 되는 순간 해당 채팅방과 대화 내역은 서버에서 즉시 자동 삭제**됩니다.

#### 클라이언트 -> 서버 메시지 전송 포맷
```json
{
  "type": "chat",
  "content": "안녕하세요!"
}
```

#### 서버 -> 클라이언트 수신 이벤트 포맷

1. **일반 채팅 및 입/퇴장 알림 (`type`: `"chat"`, `"join"`, `"leave"`)**
```json
{
  "id": 12,
  "room_id": "c41b1e28-...",
  "user_id": "550e8400-...",
  "nickname": "홍길동",
  "type": "chat",
  "content": "안녕하세요!",
  "created_at": "2026-10-19T09:25:00Z"
}
```

2. **실시간 방 인원 및 참여자 상태 (`type`: `"room_state"`)**
누군가 입장하거나 퇴장할 때마다 방 안의 모든 클라이언트에게 브로드캐스트됩니다.
```json
{
  "type": "room_state",
  "room_id": "c41b1e28-...",
  "current_users": 2,
  "max_users": 10,
  "participants": [
    { "user_id": "550e8400-...", "nickname": "홍길동" },
    { "user_id": "771a9300-...", "nickname": "이순신" }
  ]
}
```
