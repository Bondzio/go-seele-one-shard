/**
*  @file
*  @copyright defined in go-seele/LICENSE
 */

package event

import (

	"github.com/seeleteam/go-seele/common"
	"github.com/seeleteam/go-seele/core/types"
)

// EmptyEvent is an empty event
var EmptyEvent interface{}

// EventHandleMethod represents an event handler
type EventHandleMethod func(e Event)

// Event is the interface of events
type Event interface {
}

type ChainHeaderChangedMsg struct {
	HeaderHash	common.Hash
	ChainNum	uint64
}

type HandleNewMinedBlockMsg struct {
	Block       *types.Block
	ChainNum    uint64
}

type HandleNewTxMsg struct {
	tx          *types.Transaction
	chainNum    uint64
}

// eventListener is a struct which defines a function as a listener
type eventListener struct {
	// Callable is a callable function
	Callable        EventHandleMethod
	IsOnceListener  bool
	IsAsyncListener bool
}
