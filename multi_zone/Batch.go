package multi_zone

import (
	"github.com/relab/hotstuff"
	// "github.com/relab/hotstuff/consensus"
	"github.com/relab/hotstuff/consensus"
)

type Batch struct {
	Parent  consensus.Hash
	NodeID  hotstuff.ID
	Cmd     consensus.Command
	Hash    consensus.Hash
	BatchID uint32
}
