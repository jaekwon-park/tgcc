-- migrations/0005_honcho_session_id.sql
-- Honcho 세션 ID 체계 통일: 토픽별 honcho_session_id 컬럼 추가
-- NULL이면 기존처럼 tgcc-topic-{ID} fallback 사용

ALTER TABLE topics ADD COLUMN honcho_session_id TEXT;

UPDATE system_meta SET value = '5' WHERE key = 'schema_version';
