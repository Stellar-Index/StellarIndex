WITH (SELECT min(ledger_seq) FROM stellar.ledgers WHERE close_time >= '2026-07-01 00:00:00' AND close_time < '2026-07-04 00:00:00') AS lo,
     (SELECT max(ledger_seq) FROM stellar.ledgers WHERE close_time >= '2026-07-01 00:00:00' AND close_time < '2026-07-04 00:00:00') AS hi
SELECT ledger_seq, close_time, tx_hash, op_index, event_index, contract_id, event_type, topic_0_sym, topics_xdr, data_xdr, in_successful_call
FROM stellar.contract_events
WHERE contract_id = 'CB4SVAWJA6TSRNOJZ7W2AWFW46D5VR4ZMFZKDIKXEINZCZEGZCJZCKMI'
  AND ledger_seq BETWEEN lo AND hi
ORDER BY ledger_seq, tx_hash, op_index, event_index
LIMIT 20
SETTINGS max_threads = 2, max_execution_time = 60
FORMAT JSONEachRow
