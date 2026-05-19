# tgcc — 설계 문서 (v0.1 MVP)

> Telegram Forum Topics ↔ Claude Code 브릿지 (Go 구현)
> ccgram의 꼬임 문제를 원천 차단하고, 구독 한도를 그대로 유지하는 설계.

## 문서 구성

| 문서 | 내용 |
|------|------|
| [01_PRD.md](./01_PRD.md) | 기획서 — 배경, 목표/비목표, 시나리오, 마일스톤 |
| [02_ARCHITECTURE.md](./02_ARCHITECTURE.md) | 시스템 다이어그램, ACL 모델, 상태 머신, SQLite 스키마 |
| [03_API.md](./03_API.md) | 텔레그램 봇 커맨드, Hook 인터페이스, CLI, 내부 HTTP API |

## 한눈에 보기

### 핵심 아이디어

1. **대화형 TUI 유지** → Anthropic 구독 한도에서만 차감 (Agent SDK 크레딧 회피)
2. **단일 source of truth (SQLite)** → 봇/tmux 어느 쪽이 재시작해도 reconcile로 정합성 회복
3. **Hook 우선** → ANSI 스크롤백 파싱 의존 최소화
4. **토픽당 goroutine + supervisor** → 한 세션 장애가 전체에 전파 안 됨

### v0.1 범위

- ✅ 페어링 코드 + allowlist (개인용)
- ✅ 토픽 ↔ 세션 1:1 매핑
- ✅ spawn / kill / resume / crash 자동 복구
- ✅ Stop / Notification / SessionStart hook 수신
- ✅ 단일 Go 바이너리

### v0.2 이후

- PreToolUse hook → 인라인 키보드 권한 승인
- 파일 첨부 송수신
- 음성 전사 (Whisper)
- 사용량 추적 / 메트릭 / 대시보드
- 다중 사용자 RBAC

### 비용 모델

| 사용 방식 | 차감 풀 | tgcc |
|-----------|---------|------|
| 대화형 `claude` TUI | 구독 한도 ✅ | **이 방식 사용** |
| `claude -p` / SDK | Agent SDK 크레딧 (6/15부터) | 사용 안 함 |
| API 키 직접 | pay-as-you-go | 사용 안 함 |

## 다음 단계

설계 검토 후 M1부터 구현 시작:

- M1 (1주): 봇 폴링 + allowlist + 페어링 + SQLite 초기화
- M2 (1주): tmux 어댑터 + spawn/kill + 토픽 바인딩
- M3 (1주): Reconciler + Supervisor + 자동 복구
- M4 (1주): Hook 통합 + Stop/Notification 라우팅
- M5 (0.5주): 빌드·도큐먼트·릴리스
