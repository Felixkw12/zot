package continuous

// workerCountLocked excludes invocations blocked on owned work. They stay
// tracked and joined, but cannot exhaust the worker slots needed by children.
func (s *TaskScheduler) workerCountLocked() int {
	count := 0
	for id := range s.active {
		if s.suspended[id] == 0 {
			count++
		}
	}
	return count
}

func (tc TaskContext) suspend() func() {
	s, id := tc.inv.s, tc.inv.slot
	if id == "" {
		id = tc.inv.current().ID
	}
	s.mu.Lock()
	if s.suspended == nil {
		s.suspended = map[string]int{}
	}
	s.suspended[id]++
	s.mu.Unlock()
	s.notifyCapacity()
	return func() {
		s.mu.Lock()
		s.suspended[id]--
		if s.suspended[id] == 0 {
			delete(s.suspended, id)
		}
		s.mu.Unlock()
		s.notifyCapacity()
	}
}

// parkedOnly reports whether invocations are running and every one is
// suspended on owned work, so none is making progress by itself.
func (s *TaskScheduler) parkedOnly() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.active) == 0 {
		return false
	}
	for id := range s.active {
		if s.suspended[id] == 0 {
			return false
		}
	}
	return true
}

// cancelActive cancels every running invocation. Join waits for them.
func (s *TaskScheduler) cancelActive() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, cancel := range s.active {
		cancel()
	}
}

func (s *TaskScheduler) notifyCapacity() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
