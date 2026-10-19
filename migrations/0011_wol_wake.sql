-- migrations/0011_wol_wake.sql
-- Optional Wake-on-LAN auto-retry for remote(SSH) Codex topics.
-- When a remote host is powered off, the SSH connect fails with
-- "Connection timed out" / "Connection refused". If both columns below are
-- set, tgcc sends a WOL magic packet (via wol_relay_host, which lives on the
-- same L2 subnet) and retries the SSH connection a bounded number of times.
--
-- wol_mac:        MAC address of the remote host (e.g. "a0:ad:9f:cd:5e:8a").
-- wol_relay_host: SSH host on the same subnet that broadcasts the magic packet
--                 on tgcc's behalf (e.g. "insainty@10.1.1.21").
--                 NULL/empty = no WOL retry (existing behavior).

ALTER TABLE topics ADD COLUMN wol_mac TEXT;
ALTER TABLE topics ADD COLUMN wol_relay_host TEXT;

UPDATE system_meta SET value = '11' WHERE key = 'schema_version';
