package policy

import (
	"maps"
	"sync"
	"sync/atomic"
)

// ---------- epoch counter (lock-free, monotonic, no syscall) ----------

var epochCounter uint64

func nextEpoch() uint64 { return atomic.AddUint64(&epochCounter, 1) }

// ---------- radix node ----------

// radixNode is a compressed trie node for multi-tenant prefix tracking.
// children is indexed by the first byte of the child's text segment — O(1) lookup.
type radixNode struct {
	text     string
	children [256]*radixNode
	nChild   int               // number of non-nil children
	tenants  map[string]uint64 // instance_id → last access epoch
	parent   *radixNode
}

var nodePool = sync.Pool{New: func() any { return new(radixNode) }}

func acquireNode(text string, parent *radixNode) *radixNode {
	n := nodePool.Get().(*radixNode)
	n.text = text
	n.children = [256]*radixNode{}
	n.nChild = 0
	n.tenants = make(map[string]uint64, 2)
	n.parent = parent
	return n
}

func releaseNode(n *radixNode) {
	if n == nil {
		return
	}
	n.text = ""
	n.parent = nil
	n.tenants = nil
	nodePool.Put(n)
}

// isLeafForTenant returns true if no child contains the given tenant.
func (n *radixNode) isLeafForTenant(tenant string) bool {
	if _, ok := n.tenants[tenant]; !ok {
		return false
	}
	for i := range n.children {
		c := n.children[i]
		if c == nil {
			continue
		}
		if _, ok := c.tenants[tenant]; ok {
			return false
		}
	}
	return true
}

// ---------- PrefixMatchResult ----------

// PrefixMatchResult contains the result of a prefix match query against the tree.
type PrefixMatchResult struct {
	Tenant           string // instance_id with the deepest match
	MatchedByteCount int    // bytes matched
	InputByteCount   int    // total input bytes
}

// MatchRate returns the fraction of input matched (0.0 – 1.0).
func (r PrefixMatchResult) MatchRate() float64 {
	if r.InputByteCount == 0 {
		return 0
	}
	return float64(r.MatchedByteCount) / float64(r.InputByteCount)
}

// ---------- RadixTree ----------

// RadixTree is a multi-tenant compressed trie for tracking per-instance KV cache prefixes.
// Designed for single-writer (event-loop) + concurrent readers (monitoring).
// The external caller is responsible for synchronization if needed.
type RadixTree struct {
	root            *radixNode
	nodeCount       int64
	tenantByteCount map[string]int64 // instance_id → total bytes tracked
}

// NewRadixTree creates an empty radix tree.
func NewRadixTree() *RadixTree {
	root := acquireNode("", nil)
	return &RadixTree{
		root:            root,
		nodeCount:       1,
		tenantByteCount: make(map[string]int64, 16),
	}
}

// NodeCount returns the current number of nodes (root included).
func (t *RadixTree) NodeCount() int64 { return t.nodeCount }

// TenantByteCount returns the total bytes tracked for a given tenant.
func (t *RadixTree) TenantByteCount(tenant string) int64 {
	return t.tenantByteCount[tenant]
}

// ---------- Insert ----------

// Insert adds text to the tree under the given tenant, updating access epochs along the path.
// epoch is caller-provided so that an entire batch can share one epoch (avoids per-call overhead).
func (t *RadixTree) Insert(text string, tenant string, epoch uint64) {
	if len(text) == 0 {
		return
	}

	// Ensure tenant exists at root.
	if _, ok := t.root.tenants[tenant]; !ok {
		t.root.tenants[tenant] = 0
	}
	if _, ok := t.tenantByteCount[tenant]; !ok {
		t.tenantByteCount[tenant] = 0
	}

	remaining := text
	prev := t.root

	for len(remaining) > 0 {
		firstByte := remaining[0]
		child := prev.children[firstByte]

		if child == nil {
			// No child for this byte — create leaf with remaining text.
			leaf := acquireNode(remaining, prev)
			leaf.tenants[tenant] = epoch
			t.tenantByteCount[tenant] += int64(len(remaining))
			prev.children[firstByte] = leaf
			prev.nChild++
			t.nodeCount++
			return
		}

		// Child exists — find shared prefix length (byte-level).
		childText := child.text
		shared := sharedPrefixLen(remaining, childText)

		if shared < len(childText) {
			// Partial match — split the existing child.
			//
			// Before: prev → child("abcdef")
			// After:  prev → mid("abc") → child("def")
			//                           ↘ (if remaining has more after "abc")
			mid := acquireNode(childText[:shared], prev)
			// mid inherits child's tenants (shallow copy epochs).
			maps.Copy(mid.tenants, child.tenants)
			mid.children[childText[shared]] = child
			mid.nChild = 1
			child.text = childText[shared:]
			child.parent = mid
			t.nodeCount++

			// Replace child in prev.
			prev.children[firstByte] = mid

			// Ensure tenant exists on mid node.
			if _, ok := mid.tenants[tenant]; !ok {
				mid.tenants[tenant] = 0
				t.tenantByteCount[tenant] += int64(shared)
			}

			remaining = remaining[shared:]
			prev = mid
			continue
		}

		// Full match with child text — traverse deeper.
		if _, ok := child.tenants[tenant]; !ok {
			child.tenants[tenant] = 0
			t.tenantByteCount[tenant] += int64(len(childText))
		}
		remaining = remaining[shared:]
		prev = child
	}

	// Exhausted remaining — prev is the terminal node. Update epoch.
	prev.tenants[tenant] = epoch
}

// ---------- PrefixMatch ----------

// PrefixMatch walks the tree to find the tenant with the deepest cached prefix for text.
// Returns empty Tenant if the tree has no data.
func (t *RadixTree) PrefixMatch(text string) PrefixMatchResult {
	result := PrefixMatchResult{InputByteCount: len(text)}
	if len(text) == 0 {
		return result
	}

	remaining := text
	matchedBytes := 0
	cur := t.root

	for len(remaining) > 0 {
		firstByte := remaining[0]
		child := cur.children[firstByte]
		if child == nil {
			break
		}

		shared := sharedPrefixLen(remaining, child.text)
		matchedBytes += shared
		cur = child

		if shared < len(child.text) {
			// Partial match inside this node.
			break
		}
		remaining = remaining[shared:]
	}

	result.MatchedByteCount = matchedBytes

	// Find the tenant with the highest epoch on the deepest matched node.
	// If deepest node has no tenants (shouldn't happen), walk up.
	for cur != nil {
		bestEpoch := uint64(0)
		bestTenant := ""
		for tid, ep := range cur.tenants {
			if ep > bestEpoch || bestTenant == "" {
				bestEpoch = ep
				bestTenant = tid
			}
		}
		if bestTenant != "" {
			result.Tenant = bestTenant
			return result
		}
		cur = cur.parent
	}

	return result
}

// ---------- EvictBySize ----------

// evictionEntry holds a candidate for LRU leaf eviction.
type evictionEntry struct {
	epoch  uint64
	tenant string
	node   *radixNode
}

// EvictBySize trims the tree so that each tenant's tracked bytes ≤ maxBytesPerTenant.
// Uses leaf-LRU: collect leaf-tenant pairs, sort by epoch ascending, evict oldest first.
func (t *RadixTree) EvictBySize(maxBytesPerTenant int64) {
	// Collect leaf-tenant pairs via DFS.
	type stackItem struct{ node *radixNode }
	stack := make([]stackItem, 0, 64)
	stack = append(stack, stackItem{t.root})

	// Use a min-heap (slice sorted lazily) for simplicity.
	var candidates []evictionEntry

	for len(stack) > 0 {
		item := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		n := item.node

		for i := range n.children {
			if n.children[i] != nil {
				stack = append(stack, stackItem{n.children[i]})
			}
		}

		// For each tenant that is a leaf at this node, add to candidates.
		for tid, ep := range n.tenants {
			if n.isLeafForTenant(tid) {
				candidates = append(candidates, evictionEntry{epoch: ep, tenant: tid, node: n})
			}
		}
	}

	// Sort by epoch ascending (oldest first).
	sortEvictionEntries(candidates)

	// Process eviction.
	for _, e := range candidates {
		used := t.tenantByteCount[e.tenant]
		if used <= maxBytesPerTenant {
			continue
		}

		n := e.node
		// Verify still a leaf for this tenant (may have changed during eviction).
		if !n.isLeafForTenant(e.tenant) {
			continue
		}

		// Decrement byte count and remove tenant from node.
		nodeLen := int64(len(n.text))
		t.tenantByteCount[e.tenant] -= nodeLen
		if t.tenantByteCount[e.tenant] < 0 {
			t.tenantByteCount[e.tenant] = 0
		}
		delete(n.tenants, e.tenant)

		// If node is now empty (no tenants, no children) — prune it.
		t.pruneIfEmpty(n)
	}
}

// pruneIfEmpty removes the node if it has no tenants and no children,
// and recursively checks its parent.
func (t *RadixTree) pruneIfEmpty(n *radixNode) {
	if n == t.root {
		return
	}
	if len(n.tenants) > 0 || n.nChild > 0 {
		return
	}
	parent := n.parent
	if parent != nil && len(n.text) > 0 {
		fb := n.text[0]
		if parent.children[fb] == n {
			parent.children[fb] = nil
			parent.nChild--
		}
	}
	t.nodeCount--
	releaseNode(n)

	// Check if parent can be pruned too.
	if parent != nil {
		t.pruneIfEmpty(parent)
	}
}

// ---------- RemoveTenant ----------

// RemoveTenant removes all traces of a tenant from the tree, pruning empty nodes.
func (t *RadixTree) RemoveTenant(tenant string) {
	// Collect leaves for this tenant via DFS, then remove bottom-up.
	type stackItem struct{ node *radixNode }
	stack := make([]stackItem, 0, 64)
	stack = append(stack, stackItem{t.root})

	var leaves []*radixNode

	for len(stack) > 0 {
		item := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		n := item.node

		for i := range n.children {
			if n.children[i] != nil {
				stack = append(stack, stackItem{n.children[i]})
			}
		}

		if n.isLeafForTenant(tenant) {
			leaves = append(leaves, n)
		}
	}

	// Remove bottom-up: each leaf removal may expose parent as new leaf.
	for len(leaves) > 0 {
		var nextLeaves []*radixNode
		for _, n := range leaves {
			if _, ok := n.tenants[tenant]; !ok {
				continue
			}
			nodeLen := int64(len(n.text))
			t.tenantByteCount[tenant] -= nodeLen
			delete(n.tenants, tenant)

			// Prune if empty.
			parent := n.parent
			if len(n.tenants) == 0 && n.nChild == 0 && n != t.root {
				if parent != nil && len(n.text) > 0 {
					fb := n.text[0]
					if parent.children[fb] == n {
						parent.children[fb] = nil
						parent.nChild--
					}
				}
				t.nodeCount--
				releaseNode(n)
			}

			// Check if parent became a leaf for this tenant.
			if parent != nil {
				if parent.isLeafForTenant(tenant) {
					nextLeaves = append(nextLeaves, parent)
				}
			}
		}
		leaves = nextLeaves
	}

	if t.tenantByteCount[tenant] <= 0 {
		delete(t.tenantByteCount, tenant)
	}
}

// ---------- Reset ----------

// Reset clears the entire tree, releasing all nodes.
func (t *RadixTree) Reset() {
	t.releaseSubtree(t.root)
	t.root = acquireNode("", nil)
	t.nodeCount = 1
	t.tenantByteCount = make(map[string]int64, 16)
}

// releaseSubtree recursively releases all nodes in the subtree.
func (t *RadixTree) releaseSubtree(n *radixNode) {
	if n == nil {
		return
	}
	for i := range n.children {
		if n.children[i] != nil {
			t.releaseSubtree(n.children[i])
			n.children[i] = nil
		}
	}
	releaseNode(n)
}

// ---------- helpers ----------

// sharedPrefixLen returns the number of leading bytes common to a and b.
func sharedPrefixLen(a, b string) int {
	n := min(len(b), len(a))
	for i := range n {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// sortEvictionEntries sorts by epoch ascending using insertion sort
// (candidates are typically small; avoids sort package import).
func sortEvictionEntries(s []evictionEntry) {
	for i := 1; i < len(s); i++ {
		key := s[i]
		j := i - 1
		for j >= 0 && s[j].epoch > key.epoch {
			s[j+1] = s[j]
			j--
		}
		s[j+1] = key
	}
}
