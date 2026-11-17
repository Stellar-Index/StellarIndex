-- 0228 down — no-op. The previous width was TimescaleDB's create-time
-- default, not a value any migration set, and widening chunks again would
-- only bring back the refresh tail; the 7-day setting is harmless to every
-- earlier schema. Chunks already created keep their range either way.

SELECT 1;
