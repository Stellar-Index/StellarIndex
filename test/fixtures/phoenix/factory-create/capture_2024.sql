SELECT ledger_seq, close_time, tx_hash, op_index, event_index, contract_id, event_type, topic_0_sym, topics_xdr, data_xdr, in_successful_call
FROM stellar.contract_events
WHERE contract_id = 'CB4SVAWJA6TSRNOJZ7W2AWFW46D5VR4ZMFZKDIKXEINZCZEGZCJZCKMI'
  AND ledger_seq BETWEEN 51572026 AND 51572101
ORDER BY ledger_seq, tx_hash, op_index, event_index
SETTINGS max_threads = 2, max_execution_time = 30
FORMAT JSONEachRow
