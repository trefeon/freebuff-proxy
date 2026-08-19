package reasoningcache

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultMaxEntries is the default maximum number of entries held in cache.
	DefaultMaxEntries = 10000
	// DefaultTTL is the default time-to-live for cache entries.
	DefaultTTL = 2 * time.Hour
)

// Entry stores reasoning content, signature, and metadata for a tool call or completion.
type Entry struct {
	ReasoningContent string
	Signature        string
	Model            string
	CreatedAt        time.Time
}

type cacheNode struct {
	entry       *Entry
	toolCallIDs []string
	hashKey     string
	element     *list.Element
}

// Cache is a thread-safe LRU/TTL cache for tool call reasoning content and signatures.
type Cache struct {
	mu         sync.RWMutex
	byToolID   map[string]*Entry
	byHash     map[string]*Entry
	nodes      map[*Entry]*cacheNode
	lru        *list.List
	maxEntries int
	ttl        time.Duration
}

// New creates a new Cache with the given maxEntries and ttl.
// If maxEntries <= 0, DefaultMaxEntries is used.
// If ttl <= 0, DefaultTTL is used.
func New(maxEntries int, ttl time.Duration) *Cache {
	if maxEntries <= 0 {
		maxEntries = DefaultMaxEntries
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return &Cache{
		byToolID:   make(map[string]*Entry),
		byHash:     make(map[string]*Entry),
		nodes:      make(map[*Entry]*cacheNode),
		lru:        list.New(),
		maxEntries: maxEntries,
		ttl:        ttl,
	}
}

func hashKey(content, toolCallsJSON string) string {
	if content == "" && toolCallsJSON == "" {
		return ""
	}
	h := sha256.Sum256([]byte(content + toolCallsJSON))
	return hex.EncodeToString(h[:])
}

// Put stores reasoning content and signature in the cache.
// If reasoning == "" && signature == "", Put does nothing.
// Stored entries are indexed by each non-empty toolCallID and by sha256(content + toolCallsJSON).
func (c *Cache) Put(toolCallIDs []string, content string, toolCallsJSON string, reasoning string, signature string, model string) {
	if reasoning == "" && signature == "" {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()

	// 1. Evict expired entries from the back
	if c.ttl > 0 {
		for elem := c.lru.Back(); elem != nil; {
			prev := elem.Prev()
			node := elem.Value.(*cacheNode)
			if now.Sub(node.entry.CreatedAt) > c.ttl {
				c.evictNode(node)
				elem = prev
			} else {
				break
			}
		}
	}

	// 2. Evict oldest if capacity is reached
	for c.lru.Len() >= c.maxEntries && c.lru.Len() > 0 {
		back := c.lru.Back()
		if back == nil {
			break
		}
		c.evictNode(back.Value.(*cacheNode))
	}

	// 3. Filter non-empty unique toolCallIDs
	var validIDs []string
	seen := make(map[string]bool, len(toolCallIDs))
	for _, id := range toolCallIDs {
		id = strings.TrimSpace(id)
		if id != "" && !seen[id] {
			seen[id] = true
			validIDs = append(validIDs, id)
		}
	}

	// 4. Compute hash key
	hKey := hashKey(content, toolCallsJSON)

	// 5. Create entry and node
	entry := &Entry{
		ReasoningContent: reasoning,
		Signature:        signature,
		Model:            model,
		CreatedAt:        now,
	}

	node := &cacheNode{
		entry:       entry,
		toolCallIDs: validIDs,
		hashKey:     hKey,
	}

	node.element = c.lru.PushFront(node)
	c.nodes[entry] = node

	for _, id := range validIDs {
		c.byToolID[id] = entry
	}
	if hKey != "" {
		c.byHash[hKey] = entry
	}
}

func (c *Cache) evictNode(node *cacheNode) {
	if node == nil {
		return
	}
	if node.element != nil {
		c.lru.Remove(node.element)
		node.element = nil
	}
	for _, id := range node.toolCallIDs {
		if c.byToolID[id] == node.entry {
			delete(c.byToolID, id)
		}
	}
	if node.hashKey != "" {
		if c.byHash[node.hashKey] == node.entry {
			delete(c.byHash, node.hashKey)
		}
	}
	delete(c.nodes, node.entry)
}

// Get looks up reasoning content and signature by toolID first (if non-empty), and falls back to hash(content, toolCallsJSON).
func (c *Cache) Get(toolID string, content, toolCallsJSON string) (reasoning, signature string, ok bool) {
	if c == nil {
		return "", "", false
	}
	if strings.TrimSpace(toolID) != "" {
		if r, s, found := c.GetByToolID(toolID); found {
			return r, s, true
		}
	}
	if content != "" || toolCallsJSON != "" {
		return c.GetByHash(content, toolCallsJSON)
	}
	return "", "", false
}

// GetByToolID looks up reasoning content and signature by tool_call_id.
// If the entry is expired, it is removed and ok is false.
func (c *Cache) GetByToolID(toolID string) (reasoning, signature string, ok bool) {
	toolID = strings.TrimSpace(toolID)
	if toolID == "" {
		return "", "", false
	}

	c.mu.RLock()
	entry, found := c.byToolID[toolID]
	if !found {
		c.mu.RUnlock()
		return "", "", false
	}

	if c.ttl > 0 && time.Since(entry.CreatedAt) > c.ttl {
		c.mu.RUnlock()
		c.mu.Lock()
		if e, exists := c.byToolID[toolID]; exists && c.ttl > 0 && time.Since(e.CreatedAt) > c.ttl {
			if node, hasNode := c.nodes[e]; hasNode {
				c.evictNode(node)
			} else {
				delete(c.byToolID, toolID)
			}
		}
		c.mu.Unlock()
		return "", "", false
	}

	r := entry.ReasoningContent
	s := entry.Signature
	c.mu.RUnlock()
	return r, s, true
}

// GetByHash looks up reasoning content and signature by content and toolCallsJSON.
// If the entry is expired, it is removed and ok is false.
func (c *Cache) GetByHash(content, toolCallsJSON string) (reasoning, signature string, ok bool) {
	hKey := hashKey(content, toolCallsJSON)
	if hKey == "" {
		return "", "", false
	}

	c.mu.RLock()
	entry, found := c.byHash[hKey]
	if !found {
		c.mu.RUnlock()
		return "", "", false
	}

	if c.ttl > 0 && time.Since(entry.CreatedAt) > c.ttl {
		c.mu.RUnlock()
		c.mu.Lock()
		if e, exists := c.byHash[hKey]; exists && c.ttl > 0 && time.Since(e.CreatedAt) > c.ttl {
			if node, hasNode := c.nodes[e]; hasNode {
				c.evictNode(node)
			} else {
				delete(c.byHash, hKey)
			}
		}
		c.mu.Unlock()
		return "", "", false
	}

	r := entry.ReasoningContent
	s := entry.Signature
	c.mu.RUnlock()
	return r, s, true
}

// GetEntryByToolID returns a copy of the Entry for the given tool_call_id.
func (c *Cache) GetEntryByToolID(toolID string) (*Entry, bool) {
	toolID = strings.TrimSpace(toolID)
	if toolID == "" {
		return nil, false
	}

	c.mu.RLock()
	entry, found := c.byToolID[toolID]
	if !found {
		c.mu.RUnlock()
		return nil, false
	}

	if c.ttl > 0 && time.Since(entry.CreatedAt) > c.ttl {
		c.mu.RUnlock()
		c.mu.Lock()
		if e, exists := c.byToolID[toolID]; exists && c.ttl > 0 && time.Since(e.CreatedAt) > c.ttl {
			if node, hasNode := c.nodes[e]; hasNode {
				c.evictNode(node)
			} else {
				delete(c.byToolID, toolID)
			}
		}
		c.mu.Unlock()
		return nil, false
	}

	res := *entry
	c.mu.RUnlock()
	return &res, true
}

// GetEntryByHash returns a copy of the Entry for the given content and toolCallsJSON.
func (c *Cache) GetEntryByHash(content, toolCallsJSON string) (*Entry, bool) {
	hKey := hashKey(content, toolCallsJSON)
	if hKey == "" {
		return nil, false
	}

	c.mu.RLock()
	entry, found := c.byHash[hKey]
	if !found {
		c.mu.RUnlock()
		return nil, false
	}

	if c.ttl > 0 && time.Since(entry.CreatedAt) > c.ttl {
		c.mu.RUnlock()
		c.mu.Lock()
		if e, exists := c.byHash[hKey]; exists && c.ttl > 0 && time.Since(e.CreatedAt) > c.ttl {
			if node, hasNode := c.nodes[e]; hasNode {
				c.evictNode(node)
			} else {
				delete(c.byHash, hKey)
			}
		}
		c.mu.Unlock()
		return nil, false
	}

	res := *entry
	c.mu.RUnlock()
	return &res, true
}

// Len returns the current number of cached entries.
func (c *Cache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lru.Len()
}

// Prune sweeps and removes all expired entries.
func (c *Cache) Prune() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.ttl <= 0 {
		return
	}

	now := time.Now()
	for elem := c.lru.Back(); elem != nil; {
		prev := elem.Prev()
		node := elem.Value.(*cacheNode)
		if now.Sub(node.entry.CreatedAt) > c.ttl {
			c.evictNode(node)
			elem = prev
		} else {
			break
		}
	}
}
