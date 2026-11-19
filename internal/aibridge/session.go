package aibridge

// 多轮会话记忆：只放内存，不把聊天原文写进数据库。
// 进程重启即清空，避免本地存档里留下可检索的对话记录。

import "sync"

const (
	// 私聊目标 200 条、频道 150 条；缓存按最大值兜底，真正发多少由上下文预算再裁。
	sessionMaxMessages = 220
	sessionMaxChars    = 9000
	sessionMaxKeys     = 256 // 最多同时保留的会话数
)

// SessionStore 是进程内的有界会话缓存。
type SessionStore struct {
	mu       sync.Mutex
	sessions map[string][]Message
	order    []string
}

func NewSessionStore() *SessionStore {
	return &SessionStore{sessions: map[string][]Message{}}
}

func (s *SessionStore) touchLocked(key string) {
	for i, k := range s.order {
		if k == key {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	s.order = append(s.order, key)
	for len(s.order) > sessionMaxKeys {
		old := s.order[0]
		s.order = s.order[1:]
		delete(s.sessions, old)
	}
}

// History 返回最近 n 条消息（副本）。
func (s *SessionStore) History(key string, n int) []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	msgs := s.sessions[key]
	if n <= 0 || len(msgs) <= n {
		out := make([]Message, len(msgs))
		copy(out, msgs)
		return out
	}
	out := make([]Message, n)
	copy(out, msgs[len(msgs)-n:])
	return out
}

// Append 追加一轮对话并裁剪到上限。
func (s *SessionStore) Append(key string, msgs ...Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := append(s.sessions[key], msgs...)
	if len(cur) > sessionMaxMessages {
		cur = cur[len(cur)-sessionMaxMessages:]
	}
	// 再按字符预算裁剪：从最新往前累计，超出就丢最旧。
	total, start := 0, 0
	for i := len(cur) - 1; i >= 0; i-- {
		total += len([]rune(cur[i].Content))
		if total > sessionMaxChars {
			start = i + 1
			break
		}
	}
	if start > 0 {
		cur = cur[start:]
	}
	s.sessions[key] = cur
	s.touchLocked(key)
}

// Clear 清空某个会话（后台「重置对话」可用）。
func (s *SessionStore) Clear(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, key)
}
