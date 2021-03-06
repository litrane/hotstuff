package replica

import (
	"container/list"
	"context"
	"crypto/sha256"
	"sync"

	"github.com/relab/hotstuff"

	"github.com/relab/hotstuff/consensus"
	"github.com/relab/hotstuff/internal/proto/clientpb"
	"github.com/relab/hotstuff/modules"
	"google.golang.org/protobuf/proto"
)

type Batch struct {
	Parent  consensus.Hash
	NodeID  hotstuff.ID
	Cmd     consensus.Command
	Hash    consensus.Hash
	BatchID uint32
}

func (b *Batch) ToBytes() []byte {
	buf := b.Parent[:]
	buf = append(buf, []byte(b.Cmd)...)
	buf = append(buf, []byte(b.NodeID.ToBytes())...)
	return buf
}

func NewBatch(parent consensus.Hash, id hotstuff.ID, cmd consensus.Command, batchID uint32) *consensus.BatchMsg {
	b := &consensus.BatchMsg{
		Parent:  parent,
		NodeID:  id,
		Cmd:     cmd,
		BatchID: batchID,
	}
	// cache the hash immediately because it is too racy to do it in Hash()
	b.Hash = sha256.Sum256(b.ToBytes())
	return b
}

type MultiChain struct {
	ChainPool map[hotstuff.ID]([]consensus.BatchMsg)
	ID        uint32
	NodeID    hotstuff.ID
}

func newMultiChain(id hotstuff.ID) *MultiChain {
	return &MultiChain{
		ChainPool: make(map[hotstuff.ID]([]consensus.BatchMsg)),
		ID:        0,
		NodeID:    id,
	}
}

func (c *MultiChain) add(id hotstuff.ID, b *consensus.BatchMsg) {
	c.ChainPool[id] = append(c.ChainPool[id], *b)
}

func (c *MultiChain) deleteByHash(h consensus.Hash) {
	for key, value := range c.ChainPool {
		for id, batch := range value {
			if batch.Hash == h {
				c.ChainPool[key] = c.ChainPool[key][id+1:]
				break
			}
		}
	}
}
func (c *MultiChain) pack(cache *cmdCache) *consensus.BatchMsg {
	for cache.cache.Len() <= cache.batchSize {
		//<-cache.cc
		batch := new(clientpb.Batch)
		for i := 0; i < cache.batchSize; i++ {
			elem := cache.cache.Front()
			if elem == nil {
				break
			}
			//cache.cache.Remove(elem)
			cmd := elem.Value.(*clientpb.Command)
			batch.Commands = append(batch.Commands, cmd)
		}
		ba, err := cache.marshaler.Marshal(batch)
		if err != nil {
			cache.mods.Logger().Errorf("Failed to marshal batch: %v", err)
		}

		cmd := consensus.Command(ba)
		b := new(consensus.BatchMsg)

		var genesisHash [32]byte
		if len(c.ChainPool[c.NodeID]) == 0 {
			b = NewBatch(genesisHash, c.NodeID, cmd, c.ID)
		} else {
			b = NewBatch(c.ChainPool[c.NodeID][len(c.ChainPool[c.NodeID])-1].Hash, c.NodeID, cmd, c.ID)
		}
		c.ID++
		c.add(c.NodeID, b)
		cache.mods.Logger().Infof("Faaa")
		return b
		//b.Hash=
		//c.ChainPool[cache.mods.ID()]=append(c.ChainPool[cache.mods.ID()],b)
	}
	return nil
}

type cmdCache struct {
	mut           sync.Mutex
	mods          *modules.Modules
	c             chan struct{}
	cc            chan int
	batchSize     int
	serialNumbers map[uint32]uint64 // highest proposed serial number per client ID
	cache         list.List
	marshaler     proto.MarshalOptions
	unmarshaler   proto.UnmarshalOptions
}

func newCmdCache(batchSize int) *cmdCache {
	return &cmdCache{
		c:             make(chan struct{}),
		batchSize:     batchSize,
		serialNumbers: make(map[uint32]uint64),
		marshaler:     proto.MarshalOptions{Deterministic: true},
		unmarshaler:   proto.UnmarshalOptions{DiscardUnknown: true},
	}
}

// InitModule gives the module access to the other modules.
func (c *cmdCache) InitModule(mods *modules.Modules) {
	c.mods = mods
}

func (c *cmdCache) addCommand(cmd *clientpb.Command) {
	c.mut.Lock()
	defer c.mut.Unlock()
	if serialNo := c.serialNumbers[cmd.GetClientID()]; serialNo >= cmd.GetSequenceNumber() {
		// command is too old
		return
	}
	c.cache.PushBack(cmd)
	//	fmt.Println(cmd.ClientID, c.mods.ID())
	if c.cache.Len() >= c.batchSize {
		// notify Get that we are ready to send a new batch.
		select {
		case c.c <- struct{}{}:
		default:
		}
	}
}

// Get returns a batch of commands to propose.
func (c *cmdCache) Get(ctx context.Context) (cmd consensus.Command, ok bool) {
	batch := new(clientpb.Batch)

	c.mut.Lock()
awaitBatch:
	// wait until we can send a new batch.
	for c.cache.Len() <= c.batchSize {
		c.mut.Unlock()
		select {
		case <-c.c:
		case <-ctx.Done():
			return
		}
		c.mut.Lock()
	}

	// Get the batch. Note that we may not be able to fill the batch, but that should be fine as long as we can send
	// at least one command.
	for i := 0; i < c.batchSize; i++ {
		elem := c.cache.Front()
		if elem == nil {
			break
		}
		c.cache.Remove(elem)
		cmd := elem.Value.(*clientpb.Command)
		if serialNo := c.serialNumbers[cmd.GetClientID()]; serialNo >= cmd.GetSequenceNumber() {
			// command is too old
			i--
			continue
		}
		batch.Commands = append(batch.Commands, cmd)
	}

	// if we still got no (new) commands, try to wait again
	if len(batch.Commands) == 0 {
		goto awaitBatch
	}

	defer c.mut.Unlock()

	// otherwise, we should have at least one command
	b, err := c.marshaler.Marshal(batch)
	if err != nil {
		c.mods.Logger().Errorf("Failed to marshal batch: %v", err)
		return "", false
	}

	cmd = consensus.Command(b)
	return cmd, true
}

// Accept returns true if the replica can accept the batch.
func (c *cmdCache) Accept(cmd consensus.Command) bool {
	batch := new(clientpb.Batch)
	err := c.unmarshaler.Unmarshal([]byte(cmd), batch)
	if err != nil {
		c.mods.Logger().Errorf("Failed to unmarshal batch: %v", err)
		return false
	}

	c.mut.Lock()
	defer c.mut.Unlock()

	for _, cmd := range batch.GetCommands() {
		if serialNo := c.serialNumbers[cmd.GetClientID()]; serialNo >= cmd.GetSequenceNumber() {
			// command is too old, can't accept
			return false
		}
	}

	return true
}

// Proposed updates the serial numbers such that we will not accept the given batch again.
func (c *cmdCache) Proposed(cmd consensus.Command) {
	batch := new(clientpb.Batch)
	err := c.unmarshaler.Unmarshal([]byte(cmd), batch)
	if err != nil {
		c.mods.Logger().Errorf("Failed to unmarshal batch: %v", err)
		return
	}

	c.mut.Lock()
	defer c.mut.Unlock()

	for _, cmd := range batch.GetCommands() {
		if serialNo := c.serialNumbers[cmd.GetClientID()]; serialNo < cmd.GetSequenceNumber() {
			c.serialNumbers[cmd.GetClientID()] = cmd.GetSequenceNumber()
		}
	}
}

var _ consensus.Acceptor = (*cmdCache)(nil)
