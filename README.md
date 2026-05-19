# tgcc

> Telegram Forum Topics ↔ Claude Code 브릿지 (Go 구현)

ccgram을 Go로 재작성하면서 토픽-세션 꼬임 문제를 원천 차단하고, Anthropic 구독 한도를 그대로 유지하는 설계.

## 왜 이걸 만드는가

- **기존 ccgram의 꼬임 문제 해결** — tmux 윈도우와 토픽 바인딩 상태가 분산되어 메시지가 텔레그램으로 안 오는 현상을 SQLite 단일 source of truth + reconcile로 차단
- **구독 한도 유지** — 2026년 6월 15일 Anthropic 정책 변경으로 `claude -p` / SDK는 별도 크레딧으로 분리됨. tgcc는 **대화형 TUI**를 외부에서 조작하는 방식이라 기존 구독 한도에서만 차감
- **토픽별 격리 메모리** — 로컬 자체 호스팅 Honcho와 통합. 토픽마다 독립된 AI peer로 분리되어 토픽 간 컨텍스트 혼동 없음

## 핵심 설계 원칙

1. 대화형 `claude` TUI를 tmux에 spawn (Agent SDK 크레딧 회피)
2. 단일 source of truth (SQLite WAL)
3. Claude Code hook을 1차 채널, capture-pane은 보조
4. 토픽당 goroutine + supervisor (장애 격리)
5. 단일 Go 바이너리 배포

## 빠른 시작

### 1. 설치

```bash
# 빌드 (Go 1.22+ 필요)
git clone https://github.com/jaekwon-park/tgcc.git
cd tgcc
make build
sudo cp bin/tgcc /usr/local/bin/
```

### 2. 초기 설정

```bash
tgcc init
```

바이너리와 같은 디렉토리에 `.env` 파일이 생성됩니다. 필수 항목을 수정하세요.

```bash
# {exe_dir}/.env
TELEGRAM_BOT_TOKEN=<@BotFather에서 발급>
TGCC_HOOK_TOKEN=<자동 생성됨 — 변경 불필요>
TGCC_LOG_LEVEL=info
TGCC_HOOK_PORT=47829
TGCC_ALLOWED_USERS=<허용할 텔레그램 user_id, 콤마 구분>  # 예: 123456789,987654321
TGCC_HOME_DIR=/home/$USER
```

> `TGCC_ALLOWED_USERS`: 텔레그램 user_id 확인 방법 — [@userinfobot](https://t.me/userinfobot) 에 /start 전송

### 3. tgcc.toml 설정 (선택, 권장)

바이너리와 같은 디렉토리에 `tgcc.toml`을 생성합니다:

```toml
[context]
soft_warn_bytes     = 80000
hard_compact_bytes  = 150000
fresh_restart_bytes = 300000
soft_warn_turns     = 60
hard_compact_turns  = 100
idle_hibernate_min  = 30

[honcho]
enabled   = true
url       = "http://localhost:8000"   # Honcho 서버 주소
workspace = "work"

# 포럼 토픽별 설정 (thread_id는 텔레그램 포럼 토픽 ID)
# workspace_path 지정 시 tgcc 시작마다 DB에 자동 sync됨
[[topic]]
thread_id         = 283
honcho_session_id = "topic-infra"
workspace_path    = "/opt/tgcc/workspace/ccgram/infra"

[[topic]]
thread_id         = 145
honcho_session_id = "topic-dev"
workspace_path    = "/opt/tgcc/workspace/ccgram/dev"
model             = "claude-sonnet-4-6"   # 토픽별 Claude 모델 지정 (선택)
```

> `thread_id` 확인 방법: 포럼 토픽에서 메시지 링크 복사 → URL 형식 `t.me/c/그룹ID/thread_id/메시지ID`

### 런타임 파일 위치

모든 런타임 파일은 바이너리와 동일한 디렉토리에 위치합니다:

```
{exe_dir}/
├── tgcc            # 바이너리
├── .env            # 시크릿 (chmod 600 권장)
├── tgcc.toml       # 설정
├── state.db        # SQLite 데이터베이스 (자동 생성)
└── migrations/     # SQL 마이그레이션 (빌드 시 자동 복사)
```

워크스페이스는 `{exe_dir}/workspace/{그룹이름}/{토픽이름}/CLAUDE.md` 형식으로 배포 시 직접 생성합니다 (`bin/` 은 gitignore 대상).

### 4. Claude Code hook 등록

tgcc가 Claude Code의 Stop/Notification 이벤트를 수신하려면 hook을 등록해야 합니다.

`~/.claude/settings.json` 에 추가:

```json
{
  "hooks": {
    "Stop": [{
      "matcher": "",
      "hooks": [{
        "type": "command",
        "command": "curl -s -X POST http://localhost:47829/hooks/stop -H 'Authorization: Bearer <TGCC_HOOK_TOKEN>' -H 'Content-Type: application/json' -d '{\"session_id\":\"$CLAUDE_SESSION_ID\",\"transcript_path\":\"$CLAUDE_TRANSCRIPT_PATH\"}'"
      }]
    }],
    "Notification": [{
      "matcher": "",
      "hooks": [{
        "type": "command",
        "command": "curl -s -X POST http://localhost:47829/hooks/notification -H 'Authorization: Bearer <TGCC_HOOK_TOKEN>' -H 'Content-Type: application/json' -d '{\"session_id\":\"$CLAUDE_SESSION_ID\",\"message\":\"$CLAUDE_NOTIFICATION\"}'"
      }]
    }]
  }
}
```

`<TGCC_HOOK_TOKEN>` 은 `{exe_dir}/.env`의 `TGCC_HOOK_TOKEN` 값으로 교체.

### 5. 페어링

```bash
# 1. 데몬 시작
tgcc serve

# 2. 텔레그램에서 봇에게 /pair DM 전송 → 6자리 코드 수신
# 3. 터미널에서 페어링 완료
tgcc pair <코드>
```

### 6. 그룹 등록

텔레그램 포럼 그룹에 봇을 초대한 뒤, 해당 토픽에서:

```
/register workspace=/path/to/workspace honcho_session=topic-infra
```

- `workspace`: Claude Code가 작업할 디렉토리 (생략 시 `TGCC_HOME_DIR`)
- `honcho_session`: Honcho 세션 ID (tgcc.toml에 매핑하면 자동 적용)

### 7. 사용법

```bash
tgcc status        # 데몬 상태 확인
tgcc version       # 버전 확인
```

**텔레그램 봇 명령어:**

| 명령 | 설명 |
|------|------|
| `/new [경로]` | 새 Claude Code 세션 시작 |
| `/resume` | 중단된 세션 재개 |
| `/stop` | 세션 정상 종료 |
| `/kill` | 세션 강제 종료 |
| `/status` | 현재 세션 상태 |
| `/list` | 활성 세션 목록 |
| `/refresh` | 세션 재시작 (컨텍스트 유지) |
| `/compact` | 컨텍스트 즉시 압축 |
| `/squash [N]` | 오래된 N개 턴 Honcho로 압축 |
| `/ctxstatus` | 컨텍스트 사용량 확인 |
| `/ctxconfig` | 토픽별 컨텍스트 임계값 설정 |
| `/model [모델명]` | 토픽 모델 확인/변경 |
| `/register` | 토픽 등록 |
| `/workspaces` | 작업 디렉토리 목록 |
| `/whoami` | 본인 정보 |
| `/help` | 명령 도움말 |

### 8. systemd 서비스 등록 (Linux)

```bash
sudo cp deployments/tgcc.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now tgcc
sudo systemctl status tgcc
```

## 빌드

```bash
make build          # Linux amd64 바이너리 → bin/tgcc
make build-mac      # macOS amd64 바이너리 → bin/tgcc-mac
make build-mac-arm  # macOS arm64 바이너리 → bin/tgcc-mac-arm64
make test           # 테스트 실행 (race detector)
make vet            # go vet
make clean          # 빌드 아티팩트 삭제
```

단일 바이너리, 의존성 없음 (`ldd` 결과 없음). `modernc.org/sqlite` 사용으로 CGO 불필요.

## 비용 모델 비교

| 사용 방식 | 차감 풀 | tgcc |
|-----------|---------|------|
| 대화형 `claude` TUI | 구독 한도 ✅ | **이 방식 사용** |
| `claude -p` / SDK | Agent SDK 크레딧 (6/15부터) | 사용 안 함 |
| API 키 직접 | pay-as-you-go | 사용 안 함 |

## 문서

| 문서 | 내용 |
|------|------|
| [docs/00_README.md](./docs/00_README.md) | 인덱스 |
| [docs/01_PRD.md](./docs/01_PRD.md) | 기획서 (목표/비목표/시나리오/마일스톤) |
| [docs/02_ARCHITECTURE.md](./docs/02_ARCHITECTURE.md) | 시스템 다이어그램, ACL 모델, 상태 머신, SQLite 스키마 |
| [docs/03_API.md](./docs/03_API.md) | 봇 커맨드, Hook 인터페이스, CLI, 내부 HTTP API |
| [docs/04_HONCHO.md](./docs/04_HONCHO.md) | Honcho 메모리 통합 (토픽별 격리) |
| [docs/05_CONTEXT_LIFECYCLE.md](./docs/05_CONTEXT_LIFECYCLE.md) | 컨텍스트 라이프사이클 정책 |

## 마일스톤

- **M1** (1주): 봇 폴링 + 페어링 + allowlist + SQLite ✅
- **M2** (1주): tmux 어댑터 + spawn/kill + 토픽 바인딩 ✅
- **M3** (1주): Reconciler + Supervisor + 자동 복구 ✅
- **M4** (1주): Hook 통합 + Stop/Notification 라우팅 ✅
- **M5** (0.5주): 빌드·문서·릴리스 ✅
- **v0.2** (1.5주): Honcho 통합 (자체 호스팅, 토픽 격리)
- **v0.3+**: TUI 대시보드, 권한 승인 UI, 파일 첨부

## 라이선스

MIT
