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

## 상태

🚧 **설계 단계** — v0.1 MVP 구현 시작 전. 문서 검토 후 코드 작성 예정.

## 문서

| 문서 | 내용 |
|------|------|
| [docs/00_README.md](./docs/00_README.md) | 인덱스 |
| [docs/01_PRD.md](./docs/01_PRD.md) | 기획서 (목표/비목표/시나리오/마일스톤) |
| [docs/02_ARCHITECTURE.md](./docs/02_ARCHITECTURE.md) | 시스템 다이어그램, ACL 모델, 상태 머신, SQLite 스키마 |
| [docs/03_API.md](./docs/03_API.md) | 봇 커맨드, Hook 인터페이스, CLI, 내부 HTTP API |
| [docs/04_HONCHO.md](./docs/04_HONCHO.md) | Honcho 메모리 통합 (토픽별 격리) |

## 비용 모델 비교

| 사용 방식 | 차감 풀 | tgcc |
|-----------|---------|------|
| 대화형 `claude` TUI | 구독 한도 ✅ | **이 방식 사용** |
| `claude -p` / SDK | Agent SDK 크레딧 (6/15부터) | 사용 안 함 |
| API 키 직접 | pay-as-you-go | 사용 안 함 |

## 마일스톤

- **M1** (1주): 봇 폴링 + 페어링 + allowlist + SQLite
- **M2** (1주): tmux 어댑터 + spawn/kill + 토픽 바인딩
- **M3** (1주): Reconciler + Supervisor + 자동 복구
- **M4** (1주): Hook 통합 + Stop/Notification 라우팅
- **M5** (0.5주): 빌드·문서·릴리스
- **v0.2** (1.5주): Honcho 통합 (자체 호스팅, 토픽 격리)
- **v0.3+**: TUI 대시보드, 권한 승인 UI, 파일 첨부

## 기여

설계 단계이므로 이슈와 토론 환영. PR은 v0.1 구현 시작 후 받습니다.

## 라이선스

미정 — 별도 LICENSE 파일 추가 예정.
