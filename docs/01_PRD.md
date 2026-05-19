# tgcc — 기획서 (PRD)

> Telegram Forum Topics ↔ Claude Code 브릿지 (Go 구현)
> 버전 0.1 · MVP 정의

---

## 1. 배경

### 1.1 해결하려는 문제

기존 `ccgram`(Node/Python 기반)으로 텔레그램 토픽 ↔ Claude Code를 연결해 사용 중이나 다음 문제가 발생함.

- **세션 꼬임**: tmux 윈도우와 토픽 바인딩 상태가 분산되어, 외부에서 tmux가 죽거나 봇이 재시작되면 메시지가 텔레그램으로 전달되지 않음.
- **ANSI 스크롤백 파싱 누락**: Claude Code TUI의 화면 재작성과 `capture-pane` 폴링이 어긋나며 출력 누락/중복 발생.
- **읽기 오프셋 불일치**: 봇 재시작·tmux 재연결 사이에 어디까지 보냈는지 추적이 깨짐.
- **세션 사망 무탐지**: Claude Code 프로세스가 죽어도 사용자에게 알림이 가지 않음 ("조용해짐" 현상).

### 1.2 비용 모델 제약

2026년 6월 15일부터 Anthropic이 구독 사용량을 두 풀로 분리:

| 풀 | 적용 대상 |
|----|----------|
| 구독 한도 (기존) | 대화형 `claude` TUI, 공식 채널 플러그인, Cowork, 웹/모바일 |
| Agent SDK 크레딧 (신규) | `claude -p`, Agent SDK, GitHub Actions, 3rd-party 에이전트 |

**비용 우위를 유지하려면 대화형 TUI를 외부에서 조작하는 방식이 유일한 합리적 선택**이며, tgcc는 이 전제를 따른다.

---

## 2. 목표 / 비목표

### 2.1 목표 (v0.1 MVP)

- **G1**: 텔레그램 Forum Topic 1개 = tmux window 1개 = Claude Code 세션 1개로 1:1 매핑.
- **G2**: 단일 사용자(나) allowlist + 페어링 코드 인증.
- **G3**: 세션 라이프사이클 완전 자동화 — spawn, kill, resume, crash 자동 복구.
- **G4**: 단일 source of truth (SQLite). 봇/tmux 어느 쪽이 재시작해도 reconcile로 상태 일치.
- **G5**: Claude Code hook을 1차 채널로 사용 (`Stop`, `Notification`). `capture-pane`은 보조.
- **G6**: 구독 한도에서만 차감 (Agent SDK 크레딧 사용 0).
- **G7**: 단일 Go 바이너리 배포.

### 2.2 비목표 (v0.1에서 제외)

- ❌ 다중 사용자 RBAC (데이터 모델은 확장 가능하게만 설계)
- ❌ `claude -p` / SDK 헤드리스 모드
- ❌ 권한 승인 인라인 키보드 UI (`PreToolUse` hook 처리는 v0.2)
- ❌ 사용량 추적·대시보드 (v0.2)
- ❌ 음성 전사·TTS (v0.3)
- ❌ 파일 첨부 송수신 (v0.2)
- ❌ 멀티 봇 인스턴스 (v1.0)

### 2.3 명시적 비범위 (영원히 안 함)

- ❌ 공식 텔레그램 플러그인과의 봇 토큰 공유 (409 Conflict 회피 위해 단독 폴링)
- ❌ OAuth 토큰 추출이나 3rd-party 우회 (ToS 위반)

---

## 3. 핵심 의사결정

| # | 결정 | 대안 | 근거 |
|---|------|------|------|
| D1 | tmux + 대화형 TUI | PTY 직접 / SDK 헤드리스 | 구독 한도 유지, 사용자가 로컬에서도 attach 가능 |
| D2 | Go 언어 | Node / Python | 단일 바이너리, goroutine으로 토픽별 격리, 메모리 효율 |
| D3 | SQLite 단일 상태 | 파일 + 메모리 | reconcile 가능, 트랜잭션, 단일 source of truth |
| D4 | hook 우선 채널 | capture-pane 폴링 우선 | JSON 구조화, ANSI 파싱 불필요, 누락 없음 |
| D5 | 봇 단독 폴링 | 공식 플러그인 위에 사이드카 | Bot API `getUpdates` 동시 호출 시 409 Conflict |
| D6 | 토픽 단위 goroutine + supervisor | 단일 이벤트 루프 | 한 토픽 장애가 전체 영향 없음 |
| D7 | 페어링 코드 인증 | 정적 user_id 설정 | UX 우수, allowlist 자동 등록 |

---

## 4. 사용자 시나리오

### S1. 최초 설정
1. 사용자가 `tgcc init` 실행 → `.env` 생성 (봇 토큰 입력).
2. `tgcc serve` 실행 → SQLite 초기화, 텔레그램 봇 폴링 시작.
3. 사용자가 텔레그램 그룹 생성 → Forum Topics 활성화 → 봇 초대.
4. 사용자가 봇에 `/pair` DM → 6자리 코드 수신.
5. 사용자가 터미널에서 `tgcc pair 123456` 입력 → user_id 자동 캡처 후 allowlist 등록.

### S2. 새 세션 생성
1. 사용자가 텔레그램 그룹에서 새 토픽 생성 (예: "api-refactor").
2. 토픽에 첫 메시지 입력 → 봇이 "워크스페이스를 선택하세요"라며 디렉토리 목록 제시.
3. 사용자가 디렉토리 선택 → tgcc가 tmux 윈도우 생성 + `claude` 실행 + 토픽-세션 바인딩 저장.
4. Claude Code의 `SessionStart` hook 발화 → 봇이 토픽에 "✅ session ready"라고 알림.

### S3. 대화
1. 사용자가 토픽에 메시지 입력 → 봇이 ACL 검증 → tmux send-keys로 Claude에 전달.
2. Claude의 `Stop` hook 발화 → 봇이 응답을 토픽에 송신.

### S4. 세션 크래시
1. Claude Code 프로세스가 OOM 등으로 죽음.
2. tgcc의 supervisor가 5초 내 감지 → 토픽에 "⚠️ session crashed, restarting..." 알림.
3. supervisor가 `claude --resume <session_id>`로 재시작 → 마지막 컨텍스트 복구.
4. 복구 성공 시 "✅ resumed" 알림.

### S5. 봇 재시작
1. tgcc 프로세스 재시작.
2. 부팅 시 SQLite에서 모든 바인딩 로드.
3. `tmux list-windows`로 실제 세션 생존 여부 확인.
4. 살아있는 세션 → 바인딩 재연결.
5. 죽은 세션 → 토픽에 "🔄 session lost, /resume to continue" 알림.

### S6. 알 수 없는 사용자 접근
1. allowlist에 없는 user_id가 봇에 메시지 → 봇이 무응답 + audit_log에 기록.
2. 페어링 코드는 미발급 상태에서만 응답 (페어링 후엔 allowlist만 작동).

### S7. 세션 종료
1. 사용자가 토픽에서 `/stop` 또는 토픽 자체를 삭제.
2. tgcc가 tmux window 종료 + 바인딩 삭제 + 토픽에 "🛑 session stopped" 알림.

---

## 5. 비기능 요구사항

| 항목 | 요구사항 |
|------|---------|
| 안정성 | 봇 재시작 후 5초 내 모든 바인딩 reconcile 완료 |
| 응답성 | 메시지 수신 → tmux 전달 < 200ms (네트워크 제외) |
| 메모리 | idle 시 50MB 이하, 토픽 10개 동시 운영 시 200MB 이하 |
| 디스크 | SQLite 파일 100MB 이하 유지 (audit_log는 90일 보관 후 삭제) |
| 보안 | 봇 토큰은 환경변수 또는 `.env` (chmod 600) |
| 로그 | structured JSON 로그, stderr 출력, log level 환경변수 |
| 가용성 | 자기 자신은 systemd 또는 launchd 재시작 의존 |
| 의존성 | tmux >= 3.0, Claude Code CLI, Go 표준 라이브러리만 (외부 SQLite 드라이버 제외) |

---

## 6. 성공 기준 (Definition of Done)

v0.1 MVP가 다음 조건을 모두 만족하면 릴리스:

- [ ] 단일 텔레그램 그룹에서 토픽 5개 동시 운영, 24시간 무중단 안정 동작
- [ ] 페어링 → allowlist 등록 → 메시지 송수신 end-to-end 동작
- [ ] tgcc 프로세스 강제 종료 후 재시작 → 모든 세션 자동 복구
- [ ] Claude Code 프로세스 강제 종료 → 5초 내 텔레그램 알림 + 자동 resume 시도
- [ ] allowlist 미등록 사용자 메시지 100% 차단 + audit_log 기록
- [ ] 단일 Go 바이너리로 macOS/Linux 양쪽에서 빌드·실행
- [ ] `tgcc --help` 만으로 사용법 파악 가능

---

## 7. 마일스톤

| 마일스톤 | 범위 | 예상 |
|---------|------|------|
| M1 — Skeleton | 봇 폴링, allowlist, 페어링 코드, SQLite 초기화 | 1주 |
| M2 — Session basics | tmux 어댑터, spawn/kill, 토픽-세션 바인딩 | 1주 |
| M3 — Reconcile & supervisor | 부팅 시 상태 동기화, 크래시 감지 자동 재시작 | 1주 |
| M4 — Hook integration | Claude Code hook HTTP 수신, Stop/Notification 라우팅 | 1주 |
| M5 — Polish | 에러 메시지, 한국어 한글 출력, 도큐먼트, 단일 바이너리 빌드 | 0.5주 |

총 4.5주 (1인 풀타임 기준).

---

## 8. 위험 요소

| 위험 | 영향 | 완화 |
|------|------|------|
| Anthropic이 TUI 외부 조작을 향후 SDK 풀로 분리 | 비용 모델 붕괴 | session adapter 인터페이스 추상화, 향후 SDK 모드로 swap 가능하게 설계 |
| tmux 버전 차이로 send-keys 동작 변화 | 메시지 미전달 | 부팅 시 `tmux -V` 체크, 3.0 미만은 거부 |
| Claude Code hook 스펙 변경 | 알림 누락 | hook 페이로드 버전 필드 검증, capture-pane 폴백 |
| 텔레그램 Forum Topics API 변경 | 토픽 매핑 깨짐 | `message_thread_id`만 의존, 토픽 메타데이터는 캐싱 |
| SQLite 파일 락 충돌 | 데이터 손실 | WAL 모드, 단일 프로세스 보장 (PID lock file) |
| 장수 세션의 context 누적 | 응답 지연, 정확도 저하, Claude 내부 강제 compaction | turn/byte 임계치 모니터링 + 자동 `/compact` + crash 시 fresh+representation 분기 — 상세는 [05_CONTEXT_LIFECYCLE.md](../docs/05_CONTEXT_LIFECYCLE.md) |
