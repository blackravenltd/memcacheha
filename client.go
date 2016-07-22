// memcacheha wraps github.com/bradfitz/gomemcache/memcache to provide HA (highly available) functionality with lazy client-side synchronization.
package memcacheha

import (
	"github.com/apitalent/memcacheha/log"
	"github.com/bradfitz/gomemcache/memcache"
	"time"
)

// VERSION is the version of this memcacheha client
const VERSION = "0.1.0"

var (
	// GET_NODES_PERIOD is the period between checking all sources for new or deprecated nodes
	GET_NODES_PERIOD time.Duration = time.Duration(10 * time.Second)
	// HEALTHCHECK_PERIOD is the period between healthchecks on nodes
	HEALTHCHECK_PERIOD time.Duration = time.Duration(5 * time.Second)
)

// Client represents the cluster client.
type Client struct {
	Nodes   *NodeList
	Sources []NodeSource
	Log     log.Logger

	Timeout time.Duration

	shutdownChan chan (int)
	running      bool
}

// New returns a new Client with the specified logger and NodeSources
func New(logger log.Logger, sources ...NodeSource) *Client {
	i := &Client{
		Nodes:        NewNodeList(),
		Sources:      sources,
		Log:          logger,
		Timeout:      100 * time.Millisecond,
		shutdownChan: make(chan (int)),
		running:      false,
	}
	return i
}

// Add writes the given item, if no value already exists for its key. ErrNotStored is returned if that condition is not met.
func (me *Client) Add(item *memcache.Item) error {
	// Get all nodes that are marked healthy
	nodes := me.Nodes.GetHealthyNodes()
	nodeCount := len(nodes)

	// Bug out early if no nodes
	if nodeCount == 0 {
		return ErrNoHealthyNodes
	}

	finishChan := make(chan (error))
	statusChan := make(chan (*NodeResponse), nodeCount)

	// Concurrently write to all healthy nodes
	for _, node := range nodes {
		node.Add(item, statusChan)
	}

	// True if any node returns ErrNotStored
	doSync := false
	// These are the nodes that don't contain the value
	var nodesToSync []*Node

	// Handle responses
	go func() {
		defer func() {
			r := recover()
			if r != nil {
				finishChan <- ErrUnknown
			}
		}()

		// Get response from all nodes
		for ; nodeCount > 0; nodeCount-- {
			response := <-statusChan
			if response.Error == memcache.ErrNotStored {
				doSync = true
			}
			if response.Error == nil {
				nodesToSync = append(nodesToSync, response.Node)
			}
			// We ignore other errors
		}

		// Where there any ErrNotStored?
		if doSync {
			if len(nodesToSync) > 0 {
				me.Log.Info("Add: Synchronising %d nodes", len(nodesToSync))
				// Re-read the original
				item, err := me.Get(item.Key)
				if err != nil {
					// Write to all sync nodes unconditionally
					for _, node := range nodesToSync {
						node.Set(item, nil)
					}
				}
			}

			finishChan <- memcache.ErrNotStored
			return
		}

		// If this happened, writes to all nodes failed
		if me.Nodes.GetHealthyNodeCount() == 0 {
			finishChan <- ErrNoHealthyNodes
			return
		}

		// All good
		finishChan <- nil
	}()

	// Return result
	return <-finishChan
}

// Set writes the given item, unconditionally.
func (me *Client) Set(item *memcache.Item) error {
	// Get all nodes that are marked healthy
	nodes := me.Nodes.GetHealthyNodes()
	nodeCount := len(nodes)

	// Bug out early if no nodes
	if nodeCount == 0 {
		return ErrNoHealthyNodes
	}

	finishChan := make(chan (error))
	statusChan := make(chan (*NodeResponse), nodeCount)

	// Concurrently write to all nodes
	for _, node := range nodes {
		node.Set(item, statusChan)
	}

	// Handle responses
	go func() {
		// Panic handler
		defer func() {
			r := recover()
			if r != nil {
				finishChan <- ErrUnknown
			}
		}()

		for ; nodeCount > 0; nodeCount-- {
			// We actually don't care about errors, Node handles them.
			<-statusChan
		}

		// If this happened, writes to all nodes failed
		if me.Nodes.GetHealthyNodeCount() == 0 {
			finishChan <- ErrNoHealthyNodes
			return
		}

		finishChan <- nil
	}()

	// Wait for final response and return
	return <-finishChan
}

// Get gets the item for the given key. ErrCacheMiss is returned for a memcache cache miss. The key must be at most 250 bytes in length.
func (me *Client) Get(key string) (*memcache.Item, error) {
	// Get all nodes that are marked healthy
	nodes := me.Nodes.GetHealthyNodes()
	nodeCount := len(nodes)

	// Bug out early if no nodes
	if nodeCount == 0 {
		return nil, ErrNoHealthyNodes
	}

	// If there are more than 2 nodes
	if nodeCount > 2 {
		// Reduce to Ceil(n/2) nodes
		nodesToRead := nodeCount / 2
		if nodesToRead*2 < nodeCount {
			nodesToRead += 1
		}
		for k := range nodes {
			if len(nodes) <= nodesToRead {
				break
			}
			delete(nodes, k)
		}
		nodeCount = len(nodes)
	}

	finishChan := make(chan (*NodeResponse))
	statusChan := make(chan (*NodeResponse), nodeCount)

	// Concurrently read from nodes
	for _, node := range nodes {
		node.Get(key, statusChan)
	}

	// These are the nodes to sync to if we get some ErrCacheMiss from requests
	var nodesToSync []*Node

	// Handle responses
	go func() {
		// Panic handler
		defer func() {
			r := recover()
			if r != nil {
				finishChan <- NewNodeResponse(nil, nil, ErrUnknown)
			}
		}()

		// Placeholder for result
		var item *memcache.Item

		// Get response from all nodes
		for ; nodeCount > 0; nodeCount-- {
			response := <-statusChan
			if response.Error == memcache.ErrCacheMiss {
				nodesToSync = append(nodesToSync, response.Node)
			}
			if response.Error == nil && response.Item != nil {
				item = response.Item
			}
		}

		// Did we find an item from any node?
		if item != nil {
			if len(nodesToSync) > 0 {
				me.Log.Info("Get: Synchronising %d nodes", len(nodesToSync))
				// Resync by writing to missing nodes
				for _, node := range nodesToSync {
					node.Set(item, nil)
				}
			}

			// Return Item
			finishChan <- NewNodeResponse(nil, item, nil)
			return
		}

		// Not found
		finishChan <- NewNodeResponse(nil, nil, memcache.ErrCacheMiss)
	}()

	// Wait for aggregate response
	res := <-finishChan

	return res.Item, res.Error
}

// Delete deletes the item with the provided key. The error ErrCacheMiss is returned if the item didn't already exist in the cache.
func (me *Client) Delete(key string) error {
	// Get all nodes that are marked healthy
	nodes := me.Nodes.GetHealthyNodes()
	nodeCount := len(nodes)

	// Bug out early if no nodes
	if len(nodes) == 0 {
		return ErrNoHealthyNodes
	}

	finishChan := make(chan (error))
	statusChan := make(chan (*NodeResponse), nodeCount)

	// Concurrently delete from all nodes
	for _, node := range nodes {
		node.Delete(key, statusChan)
	}

	// If any node returns ErrCacheMiss return this instead.
	var errToReturn error = nil

	// Handle responses
	go func() {
		// Panic handler
		defer func() {
			r := recover()
			if r != nil {
				finishChan <- ErrUnknown
			}
		}()

		for ; nodeCount > 0; nodeCount-- {
			response := <-statusChan
			if response.Error == memcache.ErrCacheMiss {
				errToReturn = memcache.ErrCacheMiss
			}
		}

		// If this happened, writes to all nodes failed
		if me.Nodes.GetHealthyNodeCount() == 0 {
			finishChan <- ErrNoHealthyNodes
			return
		}

		finishChan <- errToReturn
	}()

	return <-finishChan
}

// Touch updates the expiry for the given key. The seconds parameter is either a Unix timestamp or,
// if seconds is less than 1 month, the number of seconds into the future at which time the item will expire.
// ErrCacheMiss is returned if the key is not in the cache. The key must be at most 250 bytes in length.
func (me *Client) Touch(key string, seconds int32) error {
	// Get all nodes that are marked healthy
	nodes := me.Nodes.GetHealthyNodes()
	nodeCount := len(nodes)

	// Bug out early if no nodes
	if len(nodes) == 0 {
		return ErrNoHealthyNodes
	}

	finishChan := make(chan (error))
	statusChan := make(chan (*NodeResponse), nodeCount)

	// Concurrently delete from all nodes
	for _, node := range nodes {
		node.Touch(key, seconds, statusChan)
	}

	// If any node returns ErrCacheMiss return this instead.
	var errToReturn error = nil

	// Handle responses
	go func() {
		// Panic handler
		defer func() {
			r := recover()
			if r != nil {
				finishChan <- ErrUnknown
			}
		}()

		for ; nodeCount > 0; nodeCount-- {
			response := <-statusChan
			if response.Error == memcache.ErrCacheMiss {
				errToReturn = memcache.ErrCacheMiss
			}
		}

		// If this happened, writes to all nodes failed
		if me.Nodes.GetHealthyNodeCount() == 0 {
			finishChan <- ErrNoHealthyNodes
			return
		}

		finishChan <- errToReturn
	}()

	return <-finishChan
}

// Start the Client client. This should be called before any operations are called.
func (me *Client) Start() error {
	if me.running != false {
		return ErrAlreadyRunning
	}
	go me.runloop()
	return nil
}

func (me *Client) runloop() {
	me.Log.Info("Running")
	timerChannel := time.After(time.Duration(time.Second))
	lastGetNodes := time.Time{}
	lastHealthCheck := time.Time{}
	me.running = true

	for {
		select {
		case <-timerChannel:
			now := time.Now()

			if lastGetNodes.Add(GET_NODES_PERIOD).Before(now) {
				me.GetNodes()
				lastGetNodes = time.Now()
			}

			if lastHealthCheck.Add(HEALTHCHECK_PERIOD).Before(now) {
				me.HealthCheck()
				lastHealthCheck = time.Now()
			}

			timerChannel = time.After(time.Duration(time.Second / 10))

		case <-me.shutdownChan:
			me.running = false
			me.Log.Info("Stopped")
			me.shutdownChan <- 2
			return
		}
	}

}

// GetNodes updates the list of nodes in the client from the configured sources.
func (me *Client) GetNodes() {
	incomingNodes := map[string]bool{}

	for _, source := range me.Sources {
		nodes, err := source.GetNodes()
		if err != nil {
			me.Log.Error("GetNodes: Source Error: %s", err)
			return
		}

		// Added Nodes
		for _, nodeAddr := range nodes {
			incomingNodes[nodeAddr] = true
			if !me.Nodes.Exists(nodeAddr) {
				me.Log.Info("GetNodes: Node Added %s", nodeAddr)
				node := NewNode(me.Log, nodeAddr, me.Timeout)
				me.Nodes.Add(node)
				node.HealthCheck()
			}
		}
	}

	// Removed nodes
	for nodeAddr := range me.Nodes.Nodes {
		if _, found := incomingNodes[nodeAddr]; !found {
			me.Log.Info("GetNodes: Node Removed %s", nodeAddr)
			delete(me.Nodes.Nodes, nodeAddr)
		}
	}
}

// HealthCheck performs a healthcheck on all nodes.
func (me *Client) HealthCheck() error {
	for _, node := range me.Nodes.Nodes {
		_, err := node.HealthCheck()
		if err != nil {
			return err
		}
	}
	return nil
}

// Stop the Client client.
func (me *Client) Stop() error {
	if me.running != true {
		return ErrAlreadyRunning
	}
	me.shutdownChan <- 1
	<-me.shutdownChan
	return nil
}
