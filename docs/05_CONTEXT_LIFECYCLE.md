# tgcc — 컨텍스트 생애주기 (Context Lifecycle)

> 장수 세션의 컨텍스트 누적 문제를 다루는 정책 문서.
> v0.1의 미해결 약점을 명시화하고, v0.2 Honcho 통합까지의 단계적 완화책을 정의.

---

## 1. 문제 정의

기존 v0.1 설계는 세션 크래시 시 `claude --resume <claude_session_id>`로 풀 컨텍스트를 복구한다. 토픽이 오래 유지될수록 다음 문제 발생:

- transcript 누적 → Claude의 context window 포화
- 매 턴마다 전체 prior context 비용 (구독 한도라도 응답 지연·attention 희석)
- 결국 Claude Code 내부 자동 compaction 강제 또는 응답 거부
- 24시간 DoD를 넘어 며칠~몇 주 단위 운용 시 신뢰성 급락

v0.1 MVP는 "1주일 이내 짧은 세션"을 묵시적으로 가정했으나, 실제 사용 패턴에선 토픽이 장기 생존하는 게 자연스럽다.

---

## 2. 세 가지 완화 레버

| 레벨 | 방식 | 컨텍스트 보존도 | 부담 |
|------|------|---------------|------|
| L0 (현재 v0.1) | `claude --resume` 풀 복구 | 100% (raw) | 누적 폭주 |
| L1 | Claude Code의 `/compact` 자동 트리거 | 요약된 ~90% | 토큰 절약, 일부 손실 |
| L2 | 새 세션 + Honcho representation 주입 | 의미 보존, 디테일 손실 | 매우 가벼움 |
| L3 | 새 세션 + dialectic chat 결과만 주입 | 토픽 핵심만 | 가장 가벼움 |

v0.1: L0만 가능.
v0.1.x: L1 추가 (Honcho 의존 없음).
v0.2: L2/L3 가능 (Honcho 통합 후).

---

## 3. 정량 임계치

### 3.1 모니터링 신호

`Stop` hook이 들어올 때마다 갱신:

```sql
ALTER TABLE sessions ADD COLUMN transcript_bytes INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN turn_count       INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN compact_count    INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN last_compact_at  INTEGER;
```

신호 출처:
- `transcript_bytes`: hook 페이로드의 `transcript_path` 파일 stat
- `turn_count`: `Stop` hook 수신 횟수
- `compact_count`: tgcc가 `/compact`를 송신한 횟수

### 3.2 임계치 (기본값, `tgcc.toml`에서 조정)

```toml
[context]
soft_warn_bytes      = 80_000     # 토픽에 권장 알림
hard_compact_bytes   = 150_000    # 자동 /compact 송신
soft_warn_turns      = 60
hard_compact_turns   = 100
fresh_restart_bytes  = 300_000    # crash 시 --resume 대신 fresh 분기
idle_hibernate_min   = 30         # 이 시간 idle이면 프로세스 kill
```

### 3.3 임계 도달 시 동작

| 조건 | 동작 |
|------|------|
| `bytes >= soft_warn` | 토픽에 "💡 컨텍스트가 길어졌습니다. `/compact` 또는 `/refresh` 권장" 알림 (1회만) |
| `bytes >= hard_compact` | tmux send-keys로 자동 `/compact\n` 송신, `compact_count++` |
| `bytes >= fresh_restart` AND crash | `--resume` 대신 fresh 세션 분기 (§5) |
| `idle_minutes >= idle_hibernate` | 프로세스 kill, status=hibernated. 다음 메시지에 fresh 세션 + 메모리 주입 |

---

## 4. 자동 compact 정책

### 4.1 흐름

```
Stop hook 도착
  → transcript_bytes, turn_count 갱신
  → 임계 검사
  → hard_compact 초과:
       a. tmux send-keys "/compact\n" 송신
       b. status=compacting 임시 설정
       c. 다음 Stop hook (Claude의 compact 응답) 도착까지 대기
       d. transcript_bytes 재측정, compact_count++, last_compact_at=now
       e. status=active 복귀
```

### 4.2 안전장치

- 같은 세션에서 5분 이내 중복 트리거 금지 (compact 자체 비용 비싸므로)
- `compact_count >= 3`이면 자동 trigger 중단, 토픽에 "🔁 잦은 compact 감지 — `/refresh` 권장" 알림
- 사용자가 직접 `/compact` 입력한 흔적이 transcript에 있으면 자동 trigger skip (24시간 cool-down)

---

## 5. Crash 복구 분기 (가장 중요한 변경)

기존 v0.1 supervisor는 항상 `claude --resume <id>`. 신규 분기:

```
on_crash(session):
    bytes = stat(transcript_path).size

    if bytes < fresh_restart_bytes AND turn_count < 100:
        # 기존 경로 — 가벼운 세션
        claude --resume <claude_session_id>

    elif honcho.enabled:
        # L2: fresh + representation 주입
        ctx = honcho.get_representation(peer, session)
        write_to_workspace(".tgcc/RESUME_CONTEXT.md", ctx + last_5_turns_summary)
        claude (fresh)
        # CLAUDE.md 또는 첫 메시지로 RESUME_CONTEXT.md 참조 지시
        notify_topic("🔄 fresh session + 메모리 복구 (transcript 너무 큼)")

    else:
        # Honcho 없는 v0.1.x: 마지막 N턴만 요약해 첫 메시지로 주입
        summary = summarize_last_n_turns(transcript_path, n=10)
        claude (fresh)
        send_keys_after_ready(summary)
        notify_topic("🔄 fresh session — 마지막 10턴 요약만 복구")
```

sessions row 처리:
- 기존 row를 `status=stopped` + `archived_at` 설정해 보존 (transcript 추적용)
- 새 row INSERT — 같은 `topic_id` (UNIQUE 충돌 회피 위해 archive 컬럼 추가):

```sql
ALTER TABLE sessions ADD COLUMN archived_at INTEGER;
-- UNIQUE(topic_id) → UNIQUE(topic_id) WHERE archived_at IS NULL
DROP INDEX IF EXISTS sqlite_autoindex_sessions_1;
CREATE UNIQUE INDEX uq_sessions_active_topic ON sessions(topic_id) WHERE archived_at IS NULL;
```

---

## 6. Idle Hibernate

기존 v0.1: 5분 idle이면 메모리 캐시 정리, 프로세스는 살려둠.

신규: `idle_hibernate_min`(기본 30분) 초과 시:

```
on_idle_timeout(session):
    if not honcho.enabled:
        # 보수적 — kill 안 함. 메모리만 정리.
        return

    # 트랜스크립트가 충분히 크면 hibernate
    if session.transcript_bytes < 50_000:
        return  # 너무 작아서 hibernate 의미 없음

    flusher.enqueue(session.id, reason="idle")
    wait_for_flush_completion(timeout=10s)
    tmux kill-window <window>
    update_status(session.id, "hibernated")
    notify_topic("💤 30분 무활동 — 세션 정리됨. 메시지 보내면 메모리 복구하며 재시작")
```

다음 메시지 도착 시: §5의 L2 경로로 fresh 세션 spawn.

---

## 7. 새 봇 커맨드

| 명령 | 사용처 | 동작 |
|------|--------|------|
| `/compact` | Topic | 즉시 Claude에 `/compact` 송신 |
| `/refresh` | Topic | 현재 세션 kill + fresh 세션 + Honcho 메모리 주입 (없으면 last-N 요약) |
| `/squash N` | Topic | 가장 오래된 N턴을 dialectic chat 요약으로 압축, 최근은 raw 유지 (v0.3) |
| `/ctxstatus` | Topic | 현재 transcript_bytes, turn_count, 임계까지 여유 표시 |

---

## 8. `/ctxstatus` 응답 예시

```
📊 컨텍스트 상태

토픽: api-refactor
세션 ID: 7a3f-...
턴 수: 47 / 100 (자동 compact)
크기: 92 KB / 150 KB (자동 compact)
경고선: ✅ soft warn 통과 (80 KB)
compact 횟수: 1 (35분 전)

💡 자동 compact까지 58 KB 여유. /refresh로 즉시 정리 가능.
```

---

## 9. 위험 및 트레이드오프

| 위험 | 영향 | 완화 |
|------|------|------|
| 자동 `/compact`가 작업 흐름 중단 | 사용자가 응답 기다리는데 갑자기 compact 시작 | `Stop` hook 이후에만 트리거 (Claude가 idle한 시점) |
| fresh 분기 시 디테일 손실 | "그 변수명 뭐였지" 같은 디테일 질문에 답 못함 | Honcho representation이 fact 단위는 보존. raw 필요하면 `/list-archived`로 이전 transcript 접근 |
| Hibernate 후 첫 메시지 응답 지연 | Honcho 호출 + spawn = 5~10초 | 토픽에 "💭 메모리 복구 중..." 즉시 응답 |
| compact_count 누적으로 점진적 손실 | 5번 compact하면 초기 컨텍스트 거의 사라짐 | 3회 도달 시 자동 trigger 중단, `/refresh` 권장 |
| 임계치 부적절 설정 | 너무 자주 compact / 너무 늦게 compact | 토픽별 override 가능 (v0.3): `topics.context_overrides` JSON |

---

## 10. 마일스톤 매핑

| 마일스톤 | 추가 작업 |
|---------|----------|
| **v0.1.x** (M5 직후 hotfix) | §3 모니터링 컬럼 추가, §7의 `/ctxstatus` 명령, soft_warn 알림 |
| **v0.1.x** | 자동 `/compact` 트리거 (§4), 임계치 설정 |
| **v0.2-honcho-a** | §5 crash 분기의 L2 경로 (representation 주입) |
| **v0.2-honcho-b** | §6 Idle Hibernate, `/refresh` 명령 |
| **v0.3** | `/squash` 명령, dialectic chat 통합, 토픽별 override |

---

## 11. v0.1에 최소한 들어갈 것 (권장)

전체 시스템을 v0.2까지 미루지 않고 v0.1 MVP에 다음만 우선 포함:

- [x] `sessions` 테이블에 `transcript_bytes`, `turn_count`, `compact_count` 컬럼 (스키마 변경 비용 회피)
- [x] `Stop` hook에서 모니터링 신호 갱신
- [x] `soft_warn_bytes` 초과 시 토픽에 1회 알림
- [x] `/ctxstatus` 명령

자동 compact, fresh 분기, hibernate는 v0.1.x 또는 v0.2로 안전하게 미룰 수 있다. 단 **모니터링과 사용자 가시성**은 v0.1부터 있어야 운용 데이터를 모을 수 있다.

---

## 12. 검증 시나리오

| 시나리오 | 기대 동작 |
|---------|----------|
| 50턴 진행 후 `/ctxstatus` | 정확한 bytes/turns/임계까지 여유 표시 |
| 80 KB 도달 | 토픽에 권장 알림 1회 (반복 없음) |
| 150 KB 도달 | 자동 `/compact` 송신, 5분 이내 재트리거 금지 |
| 300 KB 상태에서 크래시 | `--resume` 대신 fresh + representation 주입 |
| 30분 idle 후 메시지 | hibernate → fresh spawn + 메모리 복구, 응답 지연 알림 |
| 자동 compact 3회 누적 | 자동 trigger 중단, `/refresh` 권장 메시지 |
