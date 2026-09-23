package traefikllmgateway

// needsExpire reports whether key does NOT currently have a CONFIRMED
// EXPIRE recorded for it — the single-key shape needsExpireBatch
// (redis_store.go) generalizes to a whole incrMulti/incrAndGetMulti
// batch. Every production call site now goes through needsExpireBatch;
// this stays as a test-only helper (admin-redesign WP-G cleanup) because
// redis_store_test.go's own coverage of expireSeen/commitExpire/
// forgetExpireLocked's bookkeeping asserts against it directly, one key
// at a time, which is easier to read than threading every assertion
// through a []counterIncr batch.
func (s *redisStore) needsExpire(key string) bool {
	s.expireMu.Lock()
	defer s.expireMu.Unlock()
	_, ok := s.expireSeen[key]
	return !ok
}
