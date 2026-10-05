SELECT ledger_seq, close_time, tx_hash, op_index, event_index, contract_id, event_type, topic_0_sym, topics_xdr, data_xdr, in_successful_call
FROM stellar.contract_events
WHERE ledger_seq IN (51612222, 51708902, 52428304, 52428305, 52430677, 52503940, 52504175, 52833018, 52850024, 52850307,
                     54591598, 55458184, 55570394, 55570395, 55570396, 55570549, 55803090, 55803349, 62625124)
  AND contract_id IN ('CAQF5KNOFIGRI24NQRRGUPD46Q45MGMXZMRTQFXS25Y4NZVNPT34GM6S',
                      'CBP7NO6F7FRDHSOFQBT2L2UWYIZ2PU76JKVRYAQTG3KZSQLYAOKIF2WB',
                      'CDE65QK2ROZ32V2LVLBOKYPX47TYMYO37Z6ASQTBRTBNK53C7C6QF4Y7',
                      'CDVQVKOY2YSXS2IC7KN6MNASSHPAO7UN2UR2ON4OI2SKMFJNVAMDX6DP')
  AND topic_0_sym IN ('new_auction', 'fill_auction', 'bad_debt')
ORDER BY ledger_seq, tx_hash, op_index, event_index
LIMIT 1 BY ledger_seq, tx_hash, op_index, event_index
SETTINGS max_threads = 2, max_execution_time = 120
FORMAT JSONEachRow
