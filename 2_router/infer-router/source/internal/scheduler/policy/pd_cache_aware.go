package policy

import (
	"context"
	"encoding/binary"
	"hash/fnv"
	"math"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/yzx/rl-router/internal/domain"
)

// CacheAwareConfig holds configuration for the cache-aware prefill strategy.
type CacheAwareConfig struct {
	BalanceAbsThreshold float64 // absolute load difference threshold for imbalance detection
	BalanceRelThreshold float64 // relative load ratio threshold for imbalance detection
	HitRatioWeight      float64 // weight for cache hit ratio in scoring
	LoadBalanceWeight   float64 // weight for load balance in scoring
	CacheBlockSize      int     // token block size for hashing
}

// PDCacheAwarePolicy implements cache-aware scheduling for PD separation mode
// using splitwise.PrefillCacheStrategy with block-hash radix tree and weighted scoring.
// Distinct from the centralized "cache_aware" so that prefill and decode each get
// independent state.
type PDCacheAwarePolicy struct {
	strategy      *PrefillCacheStrategy
	counterMgr    *CounterManager
	pendingTokens map[string][]uint64 // instanceID → FIFO of estimated token counts (event-loop only)
	cfg           *CacheAwareConfig
	logger        *zap.Logger
}

func NewPDCacheAwarePolicy(cfg CacheAwarePolicyConfig) *PDCacheAwarePolicy {
	swCfg := &CacheAwareConfig{
		BalanceAbsThreshold: float64(cfg.BalanceAbsThreshold),
		BalanceRelThreshold: cfg.BalanceRelThreshold,
		HitRatioWeight:      cfg.HitRatioWeight,
		LoadBalanceWeight:   cfg.LoadBalanceWeight,
		CacheBlockSize:      cfg.CacheBlockSize,
	}
	if swCfg.BalanceAbsThreshold <= 0 {
		swCfg.BalanceAbsThreshold = 30.0
	}
	if swCfg.BalanceRelThreshold <= 0 {
		swCfg.BalanceRelThreshold = 0.01
	}
	if swCfg.HitRatioWeight <= 0 {
		swCfg.HitRatioWeight = 1.0
	}
	if swCfg.LoadBalanceWeight <= 0 {
		swCfg.LoadBalanceWeight = 0.5
	}
	if swCfg.CacheBlockSize <= 0 {
		swCfg.CacheBlockSize = 64
	}
	return &PDCacheAwarePolicy{
		strategy:      NewPrefillCacheStrategy(swCfg),
		counterMgr:    NewCounterManager(),
		pendingTokens: make(map[string][]uint64),
		cfg:           swCfg,
		logger:        zap.NewNop(),
	}
}

func (p *PDCacheAwarePolicy) Name() Name { return NamePDCacheAware }

func (p *PDCacheAwarePolicy) Select(_ context.Context, req *domain.RouteContext, nodes map[string]*domain.NodeState) (*domain.Instance, error) {
	if len(nodes) == 0 {
		return nil, ErrNoInstances
	}

	// Extract available instances.
	instances := make([]*domain.Instance, 0, len(nodes))
	for _, ns := range nodes {
		if !ns.LoadAvailable() {
			continue
		}
		instances = append(instances, ns.Instance)
	}
	if len(instances) == 0 {
		return nil, ErrNoInstances
	}

	tokens := req.RequestTokenIDs
	if len(tokens) == 0 {
		tokens = charsToTokens(req.RequestText)
	}

	// Delegate to splitwise cache-aware selection.
	selected := p.strategy.CacheAwareSelectByTokens(p.logger, instances, tokens, p.counterMgr, req.SessionID)
	if selected == nil {
		return nil, ErrNoInstances
	}

	// Increment counters for load tracking.
	p.counterMgr.GetOrCreateCounter(selected.ID).Inc()
	var estimatedTokens uint64
	if len(req.RequestTokenIDs) > 0 {
		estimatedTokens = uint64(len(req.RequestTokenIDs))
	} else {
		estimatedTokens = EstimateTokens(req.RequestText)
	}
	if estimatedTokens > 0 {
		p.counterMgr.GetOrCreateTokenCounter(selected.ID).Add(estimatedTokens)
	}
	p.pendingTokens[selected.ID] = append(p.pendingTokens[selected.ID], estimatedTokens)

	return selected, nil
}

func (p *PDCacheAwarePolicy) Feedback(instanceID string, _ *domain.CostMetrics) {
	// Release request counter.
	p.counterMgr.Release(instanceID)
	// Release token counter (FIFO).
	if q := p.pendingTokens[instanceID]; len(q) > 0 {
		tokens := q[0]
		p.pendingTokens[instanceID] = q[1:]
		if len(p.pendingTokens[instanceID]) == 0 {
			delete(p.pendingTokens, instanceID)
		}
		if tokens > 0 {
			p.counterMgr.GetOrCreateTokenCounter(instanceID).Sub(tokens)
		}
	}
}

// Reset stops the old strategy and recreates all state. Implements Resettable.
func (p *PDCacheAwarePolicy) Reset() {
	if p.strategy != nil {
		p.strategy.Stop()
	}
	p.strategy = NewPrefillCacheStrategy(p.cfg)
	p.counterMgr = NewCounterManager()
	clear(p.pendingTokens)
}

// RemoveSession is a no-op — PD cache-aware policy doesn't use session bindings.
func (p *PDCacheAwarePolicy) RemoveSession(_ string) {}

// DefaultCacheAwareConfig returns reasonable default configuration.
func DefaultCacheAwareConfig() *CacheAwareConfig {
	return &CacheAwareConfig{
		BalanceAbsThreshold: 30.0,
		BalanceRelThreshold: 0.01,
		HitRatioWeight:      1.0,
		LoadBalanceWeight:   0.5,
		CacheBlockSize:      64,
	}
}

// PrefillCacheStrategy implements cache-aware prefill scheduling using a radix prefix tree.
// It tracks which prefill worker has cached which prefix, and routes new requests
// to the worker most likely to have a cache hit, balanced against load.
type PrefillCacheStrategy struct {
	absThreshold      float64
	relThreshold      float64
	hitRatioWeight    float64
	loadBalanceWeight float64
	cache             *radixPrefixCache

	// session-based cache hit tracking
	sessionWorkerMap  map[string]string // session_id -> last selected prefill instance ID
	sessionMu         sync.RWMutex
	maxSessionMapSize int // max entries before eviction
	cacheHitCount     atomic.Int64
	cacheTotalCount   atomic.Int64
}

// NewPrefillCacheStrategy creates a cache-aware prefill scheduling strategy.
func NewPrefillCacheStrategy(cfg *CacheAwareConfig) *PrefillCacheStrategy {
	if cfg == nil {
		cfg = DefaultCacheAwareConfig()
	}
	blockSize := cfg.CacheBlockSize
	if blockSize <= 0 {
		blockSize = 64
	}
	return &PrefillCacheStrategy{
		absThreshold:      cfg.BalanceAbsThreshold,
		relThreshold:      cfg.BalanceRelThreshold,
		hitRatioWeight:    cfg.HitRatioWeight,
		loadBalanceWeight: cfg.LoadBalanceWeight,
		cache:             newRadixPrefixCache(blockSize),
		sessionWorkerMap:  make(map[string]string),
		maxSessionMapSize: 10000,
	}
}

// CacheAwareSelect selects the best prefill instance using cache-aware scoring.
// Falls back to processTokensSelect when load is imbalanced or tokenization fails.
func (s *PrefillCacheStrategy) CacheAwareSelect(log *zap.Logger,
	instances []*domain.Instance, message string, counterMgr *CounterManager,
) *domain.Instance {
	return s.CacheAwareSelectByTokens(log, instances, charsToTokens(message), counterMgr, "")
}

// CacheAwareSelectByTokens selects the best prefill instance using pre-resolved tokens.
// Falls back to processTokensSelect when load is imbalanced or tokens are empty.
func (s *PrefillCacheStrategy) CacheAwareSelectByTokens(log *zap.Logger,
	instances []*domain.Instance, tokens []int, counterMgr *CounterManager, sessionID string,
) *domain.Instance {
	if len(instances) == 0 {
		return nil
	}

	// 1) Fetch node load; fallback on extreme imbalance.
	loads := s.getRunningRequests(instances, counterMgr)
	if s.isLoadImbalanced(loads) {
		log.Info("pd_cache_aware: imbalanced routing",
			zap.Float64("abs_threshold", s.absThreshold),
			zap.Float64("rel_threshold", s.relThreshold),
			zap.Int("available", len(instances)),
			zap.String("session_id", sessionID))
		return processTokensSelect(instances, counterMgr)
	}

	// 2) Check tokens.
	if len(tokens) == 0 {
		log.Info("pd_cache_aware: empty tokens fallback",
			zap.String("session_id", sessionID),
			zap.Int("available", len(instances)))
		return processTokensSelect(instances, counterMgr)
	}

	// 3) Compute prefix tree hit ratio per worker.
	workerSet := instanceIDSet(instances)
	hitRatios := s.cache.Match(tokens, workerSet)

	// 4) Choose by weighted score.
	selectedID := s.chooseByScore(instances, loads, hitRatios, counterMgr)
	if hitRatios[selectedID] <= 60 {
		if inst := processTokensSelect(instances, counterMgr); inst != nil {
			selectedID = inst.ID
		}
	}

	// 5) Record prefix for the selected worker.
	if selectedID != "" {
		s.cache.Record(tokens, selectedID)
	}

	// 6) Find and return the selected instance.
	for _, inst := range instances {
		if inst.ID == selectedID {
			log.Info("pd_cache_aware: selected worker",
				zap.String("instance_id", selectedID),
				zap.Int("hit_ratio", hitRatios[selectedID]),
				zap.String("session_id", sessionID))
			return inst
		}
	}

	// Fallback.
	return processTokensSelect(instances, counterMgr)
}

// getRunningRequests retrieves concurrent request counts from CounterManager.
func (s *PrefillCacheStrategy) getRunningRequests(instances []*domain.Instance,
	counterMgr *CounterManager,
) map[string]uint64 {
	result := make(map[string]uint64, len(instances))
	for _, inst := range instances {
		c := counterMgr.GetOrCreateCounter(inst.ID)
		result[inst.ID] = c.Get()
	}
	return result
}

// isLoadImbalanced checks if max-min difference exceeds thresholds.
func (s *PrefillCacheStrategy) isLoadImbalanced(loads map[string]uint64) bool {
	if len(loads) < 2 {
		return false
	}

	maxLoad := uint64(0)
	minLoad := uint64(math.MaxUint64)
	for _, v := range loads {
		if v > maxLoad {
			maxLoad = v
		}
		if v < minLoad {
			minLoad = v
		}
	}

	if maxLoad == minLoad {
		return false
	}

	diff := float64(maxLoad - minLoad)
	relative := diff / float64(maxLoad)

	return diff > s.absThreshold && relative > s.relThreshold
}

// chooseByScore selects instance by weighted combination of cache hit ratio and load ratio.
// score = (100 - hitRatio) / 100 * hitWeight + loadRatio * loadWeight
// Lower score is better.
func (s *PrefillCacheStrategy) chooseByScore(
	instances []*domain.Instance, loads map[string]uint64,
	hitRatios map[string]int, counterMgr *CounterManager,
) string {
	if len(instances) == 0 {
		return ""
	}

	var maxLoad uint64
	for _, inst := range instances {
		if v := loads[inst.ID]; v > maxLoad {
			maxLoad = v
		}
	}

	bestScore := math.MaxFloat64
	selected := ""

	for _, inst := range instances {
		hit := float64(hitRatios[inst.ID])
		loadRatio := 0.0
		if maxLoad > 0 {
			loadRatio = float64(loads[inst.ID]) / float64(maxLoad)
		}

		score := (100.0-hit)/100*s.hitRatioWeight + loadRatio*s.loadBalanceWeight

		if score < bestScore {
			bestScore = score
			selected = inst.ID
			continue
		}

		// Tie-breaker: prefer lower token load.
		if score == bestScore && selected != "" {
			selectedTokens := counterMgr.GetOrCreateTokenCounter(selected).Get()
			currentTokens := counterMgr.GetOrCreateTokenCounter(inst.ID).Get()
			if currentTokens < selectedTokens {
				selected = inst.ID
			}
		}
	}

	return selected
}

// GetAndResetCacheHitStats returns periodic cache hit stats and resets counters.
func (s *PrefillCacheStrategy) GetAndResetCacheHitStats() (hits int64, total int64) {
	hits = s.cacheHitCount.Swap(0)
	total = s.cacheTotalCount.Swap(0)
	return hits, total
}

// Stop shuts down the background eviction goroutine.
func (s *PrefillCacheStrategy) Stop() {
	if s.cache != nil {
		s.cache.Stop()
	}
}

// TrackSessionCacheHit tracks whether the same session_id was routed to the same prefill worker.
func (s *PrefillCacheStrategy) TrackSessionCacheHit(sessionID string, selectedWorker string) {
	if sessionID == "" {
		return
	}

	s.sessionMu.RLock()
	prevWorker, exists := s.sessionWorkerMap[sessionID]
	s.sessionMu.RUnlock()

	s.cacheTotalCount.Add(1)

	if exists && prevWorker == selectedWorker {
		s.cacheHitCount.Add(1)
	}

	s.sessionMu.Lock()
	s.sessionWorkerMap[sessionID] = selectedWorker
	// Evict random entries when map exceeds max size to prevent unbounded growth.
	if s.maxSessionMapSize > 0 && len(s.sessionWorkerMap) > s.maxSessionMapSize {
		count := 0
		for k := range s.sessionWorkerMap {
			if count >= s.maxSessionMapSize/10 {
				break
			}
			delete(s.sessionWorkerMap, k)
			count++
		}
	}
	s.sessionMu.Unlock()
}

// instanceIDSet creates a set of instance IDs from instances slice.
func instanceIDSet(instances []*domain.Instance) map[string]struct{} {
	set := make(map[string]struct{}, len(instances))
	for _, inst := range instances {
		set[inst.ID] = struct{}{}
	}
	return set
}

// charsToTokens converts message characters (runes) to token IDs for hashing.
func charsToTokens(message string) []int {
	tokens := make([]int, 0, len(message))
	for _, r := range message {
		tokens = append(tokens, int(r))
	}
	return tokens
}

// --- Radix Prefix Cache ---

type radixPrefixCache struct {
	mu               sync.RWMutex
	root             *PDradixNode
	hasher           *blockHasher
	evictionDuration time.Duration
	maxNodes         int
	nodeCount        int
	allNodes         map[*PDradixNode]struct{}
	stopCh           chan struct{} // signals eviction goroutine to exit
}

type PDradixNode struct {
	key        []uint64
	children   map[uint64]*PDradixNode
	parent     *PDradixNode
	workers    map[string]time.Time
	lastAccess time.Time
	contextLen int
}

func newRadixPrefixCache(blockSize int) *radixPrefixCache {
	if blockSize <= 0 {
		blockSize = 64
	}
	const defaultEvictionDuration = 5 * time.Minute
	const defaultMaxNodes = 200000
	root := &PDradixNode{
		key:        nil,
		children:   make(map[uint64]*PDradixNode),
		contextLen: 0,
	}
	cache := &radixPrefixCache{
		root:             root,
		hasher:           newBlockHasher(blockSize),
		evictionDuration: defaultEvictionDuration,
		maxNodes:         defaultMaxNodes,
		nodeCount:        1,
		allNodes:         map[*PDradixNode]struct{}{root: {}},
		stopCh:           make(chan struct{}),
	}
	go cache.evictionWorker(cache.evictionDuration / 2)
	return cache
}

// Match returns prefix hit rate per candidate worker (0-100).
func (c *radixPrefixCache) Match(tokens []int, allowed map[string]struct{}) map[string]int {
	result := make(map[string]int)
	hashes := c.hasher.prefixHashes(tokens)
	if len(hashes) == 0 {
		return result
	}

	c.mu.RLock()
	node, matched := c.matchPrefixHelper(c.root, hashes)
	length := matched
	for n := node; n != nil; n = n.parent {
		ratio := 0
		if len(hashes) > 0 {
			ratio = length * 100 / len(hashes)
		}
		for w := range n.workers {
			if allowed != nil {
				if _, ok := allowed[w]; !ok {
					continue
				}
			}
			if ratio > result[w] {
				result[w] = ratio
			}
		}
		if len(result) > 0 {
			break
		}
		if n.parent != nil {
			length = n.parent.contextLen
		}
	}
	c.mu.RUnlock()
	return result
}

// Record inserts block-hash prefix into radix tree and tags worker.
func (c *radixPrefixCache) Record(tokens []int, worker string) {
	if worker == "" {
		return
	}
	hashes := c.hasher.prefixHashes(tokens)
	if len(hashes) == 0 {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	node := c.insertHelper(c.root, hashes)
	now := time.Now()
	for n := node; n != nil; n = n.parent {
		if n.workers == nil {
			n.workers = make(map[string]time.Time)
		}
		n.workers[worker] = now
	}
}

func (c *radixPrefixCache) evictionWorker(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			c.evictExpired()
		case <-c.stopCh:
			return
		}
	}
}

// Stop signals the eviction goroutine to exit.
func (c *radixPrefixCache) Stop() {
	select {
	case <-c.stopCh:
		// already closed
	default:
		close(c.stopCh)
	}
}

func (c *radixPrefixCache) evictExpired() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for childKey, child := range c.root.children {
		c.evictSubtreeIfExpired(c.root, childKey, child, now)
	}
}

func (c *radixPrefixCache) evictSubtreeIfExpired(parent *PDradixNode, childKey uint64, node *PDradixNode, now time.Time) int {
	removed := 0
	for k, child := range node.children {
		removed += c.evictSubtreeIfExpired(node, k, child, now)
	}

	if parent == nil {
		return removed
	}

	if now.Sub(node.lastAccess) <= c.evictionDuration {
		return removed
	}

	delete(parent.children, childKey)
	removedSubtree := c.countSubtree(node)
	c.nodeCount -= removedSubtree
	if c.nodeCount < 1 {
		c.nodeCount = 1
	}
	c.removeSubtreeFromAll(node)
	return removed + removedSubtree
}

func (c *radixPrefixCache) countSubtree(node *PDradixNode) int {
	count := 1
	for _, child := range node.children {
		count += c.countSubtree(child)
	}
	return count
}

func (c *radixPrefixCache) removeSubtreeFromAll(node *PDradixNode) {
	if node == nil {
		return
	}
	delete(c.allNodes, node)
	for _, child := range node.children {
		c.removeSubtreeFromAll(child)
	}
	node.children = nil
	node.parent = nil
	node.workers = nil
}

func (c *radixPrefixCache) matchPrefixHelper(node *PDradixNode, hashes []uint64) (*PDradixNode, int) {
	if len(hashes) == 0 {
		return node, node.contextLen
	}

	if child, ok := node.children[hashes[0]]; ok {
		prefixLen := matchUint64Len(child.key, hashes)
		if prefixLen > 0 {
			if prefixLen == len(child.key) {
				if prefixLen == len(hashes) {
					return child, child.contextLen
				}
				if deeperNode, deeperMatched := c.matchPrefixHelper(child, hashes[prefixLen:]); deeperNode != nil && deeperMatched > 0 {
					return deeperNode, deeperMatched
				}
				return child, child.contextLen
			}
			return child, node.contextLen + prefixLen
		}
	}
	return node, node.contextLen
}

func (c *radixPrefixCache) insertHelper(node *PDradixNode, key []uint64) *PDradixNode {
	node.lastAccess = time.Now()

	if len(key) == 0 {
		return node
	}

	if child, ok := node.children[key[0]]; ok {
		prefixLen := matchUint64Len(child.key, key)

		if prefixLen == len(child.key) {
			if prefixLen == len(key) {
				child.lastAccess = time.Now()
				return child
			}
			return c.insertHelper(child, key[prefixLen:])
		}

		newNode := c.splitNode(node, child, prefixLen)
		if prefixLen == len(key) {
			return newNode
		}
		return c.insertHelper(newNode, key[prefixLen:])
	}

	newNode := newPDradixNode(node, key)
	node.children[key[0]] = newNode
	c.nodeCount++
	c.allNodes[newNode] = struct{}{}
	c.maybeEvictLocked()
	return newNode
}

func (c *radixPrefixCache) splitNode(parent *PDradixNode, child *PDradixNode, prefixLen int) *PDradixNode {
	commonKey := append([]uint64{}, child.key[:prefixLen]...)

	newNode := newPDradixNode(parent, commonKey)
	parent.children[commonKey[0]] = newNode

	child.key = append([]uint64{}, child.key[prefixLen:]...)
	child.parent = newNode
	child.contextLen = newNode.contextLen + len(child.key)

	if len(child.key) > 0 {
		newNode.children[child.key[0]] = child
	}
	return newNode
}

func (c *radixPrefixCache) maybeEvictLocked() {
	if c.maxNodes <= 0 || c.nodeCount <= c.maxNodes {
		return
	}
	c.evictExpired()
}

func newPDradixNode(parent *PDradixNode, key []uint64) *PDradixNode {
	n := &PDradixNode{
		key:        append([]uint64{}, key...),
		children:   make(map[uint64]*PDradixNode),
		parent:     parent,
		lastAccess: time.Now(),
	}
	if parent != nil {
		n.contextLen = parent.contextLen + len(key)
	} else {
		n.contextLen = len(key)
	}
	return n
}

// --- Block Hasher ---

type blockHasher struct {
	blockSize int
	seed      uint64
}

func newBlockHasher(blockSize int) *blockHasher {
	if blockSize <= 0 {
		blockSize = 64
	}
	return &blockHasher{
		blockSize: blockSize,
		seed:      rand.Uint64(),
	}
}

// prefixHashes generates parent-chain hash sequence by block.
func (h *blockHasher) prefixHashes(tokens []int) []uint64 {
	if h.blockSize <= 0 || len(tokens) < h.blockSize {
		return nil
	}
	blockCount := len(tokens) / h.blockSize
	hashes := make([]uint64, 0, blockCount)
	parent := h.seed
	buf := make([]byte, 8)

	for i := 0; i+h.blockSize <= len(tokens); i += h.blockSize {
		hasher := fnv.New64a()
		binary.LittleEndian.PutUint64(buf, parent)
		_, _ = hasher.Write(buf)

		for _, token := range tokens[i : i+h.blockSize] {
			binary.LittleEndian.PutUint64(buf, uint64(token))
			_, _ = hasher.Write(buf)
		}
		current := hasher.Sum64()
		hashes = append(hashes, current)
		parent = current
	}
	return hashes
}

func matchUint64Len(a, b []uint64) int {
	minLen := min(len(b), len(a))
	i := 0
	for i < minLen && a[i] == b[i] {
		i++
	}
	return i
}
