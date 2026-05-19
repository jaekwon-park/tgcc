# GPU 모니터링 토픽 — tgcc 워크스페이스

공통 규칙: `/home/insainty/.claude/CLAUDE.md`
리더 공통: `/home/insainty/ccbot/CLAUDE.md`
상세 규칙: `/home/insainty/ccbot/ccgram/gpu/CLAUDE.md`

## 정체성
- 맨션 ID: `@ml-leader`
- Honcho 세션: `topic-gpu`
- Multica 프로젝트: `192e24ea-1a06-43fe-8a40-77882d29c6ea`
- notify 인자: `gpu`
- `$MULTICA`: `/usr/local/bin/multica --profile ml-leader`

## 역할
GPU 서버 상태 확인, 작업 큐 관리, 학습 리소스 할당.

## 서버 접속
```bash
ssh insainty@172.16.202.193
# 폴백: ssh -J insainty@192.168.0.14 insainty@172.16.202.193
```
