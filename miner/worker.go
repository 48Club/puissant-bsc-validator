// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package miner

import (
	"errors"
	"fmt"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/parlia"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/trie"
	lru "github.com/hashicorp/golang-lru"
)

const (
	// resultQueueSize is the size of channel listening to sealing result.
	resultQueueSize = 10

	// txChanSize is the size of channel listening to NewTxsEvent.
	// The number is referenced from the size of tx pool.
	txChanSize = 4096

	// chainHeadChanSize is the size of channel listening to ChainHeadEvent.
	chainHeadChanSize = 10

	// chainSideChanSize is the size of channel listening to ChainSideEvent.
	chainSideChanSize = 10

	// sealingLogAtDepth is the number of confirmations before logging successful mining.
	sealingLogAtDepth = 11

	// minRecommitInterval is the minimal time interval to recreate the sealing block with
	// any newly arrived transactions.
	minRecommitInterval = 1 * time.Second

	// staleThreshold is the maximum depth of the acceptable stale block.
	staleThreshold = 11

	// the current 4 mining loops could have asynchronous risk of mining block with
	// save height, keep recently mined blocks to avoid double sign for safety,
	recentMinedCacheLimit = 20
)

var (
	writeBlockTimer    = metrics.NewRegisteredTimer("worker/writeblock", nil)
	finalizeBlockTimer = metrics.NewRegisteredTimer("worker/finalizeblock", nil)

	errBlockInterruptedByNewHead  = errors.New("new head arrived while building block")
	errBlockInterruptedByRecommit = errors.New("recommit interrupt while building block")
	errBlockInterruptedByTimeout  = errors.New("timeout while building block")
	errBlockInterruptedByOutOfGas = errors.New("out of gas while building block")
)

// task contains all information for consensus engine sealing and result submitting.
type task struct {
	receipts  []*types.Receipt
	state     *state.StateDB
	block     *types.Block
	createdAt time.Time
}

const (
	commitInterruptNone int32 = iota
	commitInterruptNewHead
	commitInterruptResubmit
	commitInterruptTimeout
	commitInterruptOutOfGas
)

// newWorkReq represents a request for new sealing work submitting with relative interrupt notifier.
type newWorkReq struct {
	interruptCh chan int32
	timestamp   int64
}

// getWorkReq represents a request for getting a new sealing work with provided parameters.
type getWorkReq struct {
	params *generateParams
	err    error
	result chan *types.Block
}

// worker is the main object which takes care of submitting new work to consensus engine
// and gathering the sealing result.
type worker struct {
	prefetcher  core.Prefetcher
	config      *Config
	chainConfig *params.ChainConfig
	engine      *parlia.Parlia
	eth         Backend
	chain       *core.BlockChain

	// Feeds
	pendingLogsFeed event.Feed

	// Subscriptions
	mux          *event.TypeMux
	chainHeadCh  chan core.ChainHeadEvent
	chainHeadSub event.Subscription
	chainSideCh  chan core.ChainSideEvent
	chainSideSub event.Subscription

	// Channels
	newWorkCh          chan *newWorkReq
	getWorkCh          chan *getWorkReq
	taskCh             chan *task
	resultCh           chan *types.Block
	startCh            chan struct{}
	exitCh             chan struct{}
	resubmitIntervalCh chan time.Duration

	wg sync.WaitGroup

	unconfirmed *unconfirmedBlocks // A set of locally mined blocks pending canonicalness confirmations.

	mu       sync.RWMutex // The lock used to protect the coinbase and extra fields
	coinbase common.Address
	extra    []byte

	pendingMu    sync.RWMutex
	pendingTasks map[common.Hash]*task

	snapshotMu       sync.RWMutex // The lock used to protect the snapshots below
	snapshotBlock    *types.Block
	snapshotReceipts types.Receipts
	snapshotState    *state.StateDB

	// atomic status counters
	running int32 // The indicator whether the consensus engine is running or not.

	// External functions
	isLocalBlock func(header *types.Header) bool // Function used to determine whether the specified block is mined by local miner.

	// Test hooks
	newTaskHook       func(*task)                        // Method to call upon receiving a new sealing task.
	skipSealHook      func(*task) bool                   // Method to decide whether skipping the sealing.
	fullTaskHook      func()                             // Method to call before pushing the full sealing task.
	resubmitHook      func(time.Duration, time.Duration) // Method to call upon updating resubmitting interval.
	recentMinedBlocks *lru.Cache

	// 48Club modified
	nodeAlias     string
	messengerToID int64
	messengerBot  *tgbotapi.BotAPI
}

func newWorker(config *Config, chainConfig *params.ChainConfig, engine *parlia.Parlia, eth Backend, mux *event.TypeMux, isLocalBlock func(header *types.Header) bool, init bool) *worker {
	recentMinedBlocks, _ := lru.New(recentMinedCacheLimit)
	worker := &worker{
		prefetcher:         core.NewStatePrefetcher(chainConfig, eth.BlockChain(), engine),
		config:             config,
		chainConfig:        chainConfig,
		engine:             engine,
		eth:                eth,
		mux:                mux,
		chain:              eth.BlockChain(),
		isLocalBlock:       isLocalBlock,
		pendingTasks:       make(map[common.Hash]*task),
		chainHeadCh:        make(chan core.ChainHeadEvent, chainHeadChanSize),
		chainSideCh:        make(chan core.ChainSideEvent, chainSideChanSize),
		newWorkCh:          make(chan *newWorkReq),
		getWorkCh:          make(chan *getWorkReq),
		taskCh:             make(chan *task),
		resultCh:           make(chan *types.Block, resultQueueSize),
		exitCh:             make(chan struct{}),
		startCh:            make(chan struct{}, 1),
		resubmitIntervalCh: make(chan time.Duration),
		recentMinedBlocks:  recentMinedBlocks,

		nodeAlias:     config.NodeAlias,
		messengerToID: config.TelegramToID,
	}

	if len(config.TelegramKey) > 0 && config.TelegramToID != 0 {
		if worker.nodeAlias == "" {
			worker.nodeAlias = "AnonymousMiner"
		}
		_bot, err := tgbotapi.NewBotAPI(config.TelegramKey)
		if err != nil {
			log.Error("telegram bot failed", "err", err)
		} else {
			worker.messengerBot = _bot
		}
	}
	// add msg sender
	worker.unconfirmed = newUnconfirmedBlocks(eth.BlockChain(), sealingLogAtDepth, worker.sendMessage)

	// Subscribe events for blockchain
	worker.chainHeadSub = eth.BlockChain().SubscribeChainHeadEvent(worker.chainHeadCh)
	worker.chainSideSub = eth.BlockChain().SubscribeChainSideEvent(worker.chainSideCh)

	// Sanitize recommit interval if the user-specified one is too short.
	recommit := worker.config.Recommit
	if recommit < minRecommitInterval {
		log.Warn("Sanitizing miner recommit interval", "provided", recommit, "updated", minRecommitInterval)
		recommit = minRecommitInterval
	}

	worker.wg.Add(4)
	go worker.mainLoop()
	go worker.newWorkLoop(recommit)
	go worker.resultLoop()
	go worker.taskLoop()

	// Submit first work to initialize pending state.
	if init {
		worker.startCh <- struct{}{}
	}
	return worker
}

// setEtherbase sets the etherbase used to initialize the block coinbase field.
func (w *worker) setEtherbase(addr common.Address) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.coinbase = addr
}

func (w *worker) setGasCeil(ceil uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.config.GasCeil = ceil
}

// setExtra sets the content used to initialize the block extra field.
func (w *worker) setExtra(extra []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.extra = extra
}

// setRecommitInterval updates the interval for miner sealing work recommitting.
func (w *worker) setRecommitInterval(interval time.Duration) {
	select {
	case w.resubmitIntervalCh <- interval:
	case <-w.exitCh:
	}
}

// pending returns the pending state and corresponding block.
func (w *worker) pending() (*types.Block, *state.StateDB) {
	// return a snapshot to avoid contention on currentMu mutex
	w.snapshotMu.RLock()
	defer w.snapshotMu.RUnlock()
	if w.snapshotState == nil {
		return nil, nil
	}
	return w.snapshotBlock, w.snapshotState.Copy()
}

// pendingBlock returns pending block.
func (w *worker) pendingBlock() *types.Block {
	// return a snapshot to avoid contention on currentMu mutex
	w.snapshotMu.RLock()
	defer w.snapshotMu.RUnlock()
	return w.snapshotBlock
}

// pendingBlockAndReceipts returns pending block and corresponding receipts.
func (w *worker) pendingBlockAndReceipts() (*types.Block, types.Receipts) {
	// return a snapshot to avoid contention on currentMu mutex
	w.snapshotMu.RLock()
	defer w.snapshotMu.RUnlock()
	return w.snapshotBlock, w.snapshotReceipts
}

// start sets the running status as 1 and triggers new work submitting.
func (w *worker) start() {
	go w.sendMessage("miner started", false)

	atomic.StoreInt32(&w.running, 1)
	w.startCh <- struct{}{}
}

// stop sets the running status as 0.
func (w *worker) stop() {
	go w.sendMessage("miner stopped", false)

	atomic.StoreInt32(&w.running, 0)
}

// isRunning returns an indicator whether worker is running or not.
func (w *worker) isRunning() bool {
	return atomic.LoadInt32(&w.running) == 1
}

// close terminates all background threads maintained by the worker.
// Note the worker does not support being closed multiple times.
func (w *worker) close() {
	atomic.StoreInt32(&w.running, 0)
	close(w.exitCh)
	w.wg.Wait()
}

// newWorkLoop is a standalone goroutine to submit new sealing work upon received events.
func (w *worker) newWorkLoop(recommit time.Duration) {
	defer w.wg.Done()
	var (
		interruptCh chan int32
		minRecommit = recommit // minimal resubmit interval specified by user.
		timestamp   int64      // timestamp for each round of sealing.
	)

	timer := time.NewTimer(0)
	defer timer.Stop()
	<-timer.C // discard the initial tick

	// commit aborts in-flight transaction execution with given signal and resubmits a new one.
	commit := func(reason int32) {
		if interruptCh != nil {
			// each commit work will have its own interruptCh to stop work with a reason
			interruptCh <- reason
			close(interruptCh)
		}
		interruptCh = make(chan int32, 1)
		select {
		case w.newWorkCh <- &newWorkReq{interruptCh: interruptCh, timestamp: timestamp}:
		case <-w.exitCh:
			return
		}
		timer.Reset(recommit)
	}
	// clearPending cleans the stale pending tasks.
	clearPending := func(number uint64) {
		w.pendingMu.Lock()
		for h, t := range w.pendingTasks {
			if t.block.NumberU64()+staleThreshold <= number {
				delete(w.pendingTasks, h)
			}
		}
		w.pendingMu.Unlock()
	}

	for {
		select {
		case <-w.startCh:
			clearPending(w.chain.CurrentBlock().NumberU64())
			timestamp = time.Now().Unix()
			commit(commitInterruptNewHead)

		case head := <-w.chainHeadCh:
			if !w.isRunning() {
				continue
			}
			clearPending(head.Block.NumberU64())
			timestamp = time.Now().Unix()
			signedRecent, err := w.engine.SignRecently(w.chain, head.Block)
			if err != nil {
				log.Info("Not allowed to propose block", "err", err)
				continue
			}
			if signedRecent {
				log.Info("Signed recently, must wait")
				continue
			}
			commit(commitInterruptNewHead)

		case <-timer.C:
			// If sealing is running resubmit a new work cycle periodically to pull in
			// higher priced transactions. Disable this overhead for pending blocks.
			if w.isRunning() && ((w.chainConfig.Ethash != nil) || (w.chainConfig.Clique != nil &&
				w.chainConfig.Clique.Period > 0) || (w.chainConfig.Parlia != nil && w.chainConfig.Parlia.Period > 0)) {
				// Short circuit if no new transaction arrives.
				commit(commitInterruptResubmit)
			}

		case interval := <-w.resubmitIntervalCh:
			// Adjust resubmit interval explicitly by user.
			if interval < minRecommitInterval {
				log.Warn("Sanitizing miner recommit interval", "provided", interval, "updated", minRecommitInterval)
				interval = minRecommitInterval
			}
			log.Info("Miner recommit interval update", "from", minRecommit, "to", interval)
			minRecommit, recommit = interval, interval

			if w.resubmitHook != nil {
				w.resubmitHook(minRecommit, recommit)
			}

		case <-w.exitCh:
			return
		}
	}
}

// mainLoop is responsible for generating and submitting sealing work based on
// the received event. It can support two modes: automatically generate task and
// submit it or return task according to given parameters for various proposes.
func (w *worker) mainLoop() {
	defer w.wg.Done()
	defer w.chainHeadSub.Unsubscribe()
	defer w.chainSideSub.Unsubscribe()

	for {
		select {
		case req := <-w.newWorkCh:
			w.commitWork(req.interruptCh, req.timestamp)

		case req := <-w.getWorkCh:
			req.result <- nil

		case <-w.chainSideCh:
			// Short circuit for duplicate side blocks
			continue

		// System stopped
		case <-w.exitCh:
			return
		case <-w.chainHeadSub.Err():
			return
		case <-w.chainSideSub.Err():
			return
		}
	}
}

// taskLoop is a standalone goroutine to fetch sealing task from the generator and
// push them to consensus engine.
func (w *worker) taskLoop() {
	defer w.wg.Done()
	var (
		stopCh chan struct{}
		prev   common.Hash
	)

	// interrupt aborts the in-flight sealing task.
	interrupt := func() {
		if stopCh != nil {
			close(stopCh)
			stopCh = nil
		}
	}
	for {
		select {
		case task := <-w.taskCh:
			if w.newTaskHook != nil {
				w.newTaskHook(task)
			}
			// Reject duplicate sealing work due to resubmitting.
			sealHash := w.engine.SealHash(task.block.Header())
			if sealHash == prev {
				continue
			}
			// Interrupt previous sealing operation
			interrupt()
			stopCh, prev = make(chan struct{}), sealHash

			if w.skipSealHook != nil && w.skipSealHook(task) {
				continue
			}
			w.pendingMu.Lock()
			w.pendingTasks[sealHash] = task
			w.pendingMu.Unlock()

			if err := w.engine.Seal(w.chain, task.block, w.resultCh, stopCh); err != nil {
				log.Warn("Block sealing failed", "err", err)
				w.pendingMu.Lock()
				delete(w.pendingTasks, sealHash)
				w.pendingMu.Unlock()
			}
		case <-w.exitCh:
			interrupt()
			return
		}
	}
}

// resultLoop is a standalone goroutine to handle sealing result submitting
// and flush relative data to the database.
func (w *worker) resultLoop() {
	defer w.wg.Done()
	for {
		select {
		case block := <-w.resultCh:
			// Short circuit when receiving empty result.
			if block == nil {
				continue
			}
			// Short circuit when receiving duplicate result caused by resubmitting.
			if w.chain.HasBlock(block.Hash(), block.NumberU64()) {
				continue
			}
			var (
				sealhash = w.engine.SealHash(block.Header())
				hash     = block.Hash()
			)
			w.pendingMu.RLock()
			task, exist := w.pendingTasks[sealhash]
			w.pendingMu.RUnlock()
			if !exist {
				log.Error("Block found but no relative pending task", "number", block.Number(), "sealhash", sealhash, "hash", hash)
				continue
			}
			// Different block could share same sealhash, deep copy here to prevent write-write conflict.
			var (
				receipts = make([]*types.Receipt, len(task.receipts))
				logs     []*types.Log
			)
			for i, taskReceipt := range task.receipts {
				receipt := new(types.Receipt)
				receipts[i] = receipt
				*receipt = *taskReceipt

				// add block location fields
				receipt.BlockHash = hash
				receipt.BlockNumber = block.Number()
				receipt.TransactionIndex = uint(i)

				// Update the block hash in all logs since it is now available and not when the
				// receipt/log of individual transactions were created.
				receipt.Logs = make([]*types.Log, len(taskReceipt.Logs))
				for i, taskLog := range taskReceipt.Logs {
					log := new(types.Log)
					receipt.Logs[i] = log
					*log = *taskLog
					log.BlockHash = hash
				}
				logs = append(logs, receipt.Logs...)
			}

			if prev, ok := w.recentMinedBlocks.Get(block.NumberU64()); ok {
				doubleSign := false
				prevParents, _ := prev.([]common.Hash)
				for _, prevParent := range prevParents {
					if prevParent == block.ParentHash() {
						log.Error("Reject Double Sign!!", "block", block.NumberU64(),
							"hash", block.Hash(),
							"root", block.Root(),
							"ParentHash", block.ParentHash())
						doubleSign = true
						break
					}
				}
				if doubleSign {
					continue
				}
				prevParents = append(prevParents, block.ParentHash())
				w.recentMinedBlocks.Add(block.NumberU64(), prevParents)
			} else {
				// Add() will call removeOldest internally to remove the oldest element
				// if the LRU Cache is full
				w.recentMinedBlocks.Add(block.NumberU64(), []common.Hash{block.ParentHash()})
			}

			// Commit block and state to database.
			task.state.SetExpectedStateRoot(block.Root())
			start := time.Now()
			status, err := w.chain.WriteBlockAndSetHead(block, receipts, logs, task.state, true)
			if status != core.CanonStatTy {
				if err != nil {
					log.Error("Failed writing block to chain", "err", err, "status", status)
				} else {
					log.Info("Written block as SideChain and avoid broadcasting", "status", status)
				}
				continue
			}
			writeBlockTimer.UpdateSince(start)
			log.Info("Successfully sealed new block", "number", block.Number(), "sealhash", sealhash, "hash", hash,
				"elapsed", common.PrettyDuration(time.Since(task.createdAt)))
			// Broadcast the block and announce chain insertion event
			w.mux.Post(core.NewMinedBlockEvent{Block: block})

			// Insert the block into the set of pending ones to resultLoop for confirmations
			w.unconfirmed.Insert(block.NumberU64(), block.Hash())

		case <-w.exitCh:
			return
		}
	}
}

//// makeEnv creates a new environment for the sealing block.
//func (w *worker) makeEnv(parent *types.Block, header *types.Header, coinbase common.Address,
//	prevEnv *environment) (*environment, error) {
//	// Retrieve the parent state to execute on top and start a prefetcher for
//	// the miner to speed block sealing up a bit
//	state, err := w.chain.StateAtWithSharedPool(parent.Root())
//	if err != nil {
//		return nil, err
//	}
//	if prevEnv == nil {
//		state.StartPrefetcher("miner")
//	} else {
//		state.TransferPrefetcher(prevEnv.state)
//	}
//
//	// Note the passed coinbase may be different with header.Coinbase.
//	env := &environment{
//		signer:    types.MakeSigner(w.chainConfig, header.Number),
//		state:     state,
//		coinbase:  coinbase,
//		ancestors: mapset.NewSet(),
//		family:    mapset.NewSet(),
//		header:    header,
//		uncles:    make(map[common.Hash]*types.Header),
//	}
//	// Keep track of transactions which return errors so they can be removed
//	env.tcount = 0
//	return env, nil
//}

// updateSnapshot updates pending snapshot block, receipts and state.
func (w *worker) updateSnapshot(env *core.MinerEnvironment) {
	w.snapshotMu.Lock()
	defer w.snapshotMu.Unlock()

	w.snapshotBlock = types.NewBlock(
		env.Header,
		env.PackedTxs,
		nil,
		env.Receipts,
		trie.NewStackTrie(nil),
	)
	w.snapshotReceipts = types.CopyReceipts(env.Receipts)
	w.snapshotState = env.State.Copy()
}

//// prepareWork constructs the sealing task according to the given parameters,
//// either based on the last chain head or specified parent. In this function
//// the pending transactions are not filled yet, only the empty task returned.
//func (w *worker) prepareWork(genParams *generateParams) (*environment, error) {
//	w.mu.RLock()
//	defer w.mu.RUnlock()
//
//	// Find the parent block for sealing task
//	parent := w.chain.CurrentBlock()
//	if genParams.parentHash != (common.Hash{}) {
//		parent = w.chain.GetBlockByHash(genParams.parentHash)
//	}
//	if parent == nil {
//		return nil, fmt.Errorf("missing parent")
//	}
//	// Sanity check the timestamp correctness, recap the timestamp
//	// to parent+1 if the mutation is allowed.
//	timestamp := genParams.timestamp
//	if parent.Time() >= timestamp {
//		if genParams.forceTime {
//			return nil, fmt.Errorf("invalid timestamp, parent %d given %d", parent.Time(), timestamp)
//		}
//		timestamp = parent.Time() + 1
//	}
//	// Construct the sealing block header, set the extra field if it's allowed
//	num := parent.Number()
//	header := &types.Header{
//		ParentHash: parent.Hash(),
//		Number:     num.Add(num, common.Big1),
//		GasLimit:   core.CalcGasLimit(parent.GasLimit(), w.config.GasCeil),
//		Time:       timestamp,
//		Coinbase:   genParams.coinbase,
//	}
//	if !genParams.noExtra && len(w.extra) != 0 {
//		header.Extra = w.extra
//	}
//	// Set the randomness field from the beacon chain if it's available.
//	if genParams.random != (common.Hash{}) {
//		header.MixDigest = genParams.random
//	}
//	// Set baseFee and GasLimit if we are on an EIP-1559 chain
//	if w.chainConfig.IsLondon(header.Number) {
//		header.BaseFee = misc.CalcBaseFee(w.chainConfig, parent.Header())
//		if !w.chainConfig.IsLondon(parent.Number()) {
//			parentGasLimit := parent.GasLimit() * params.ElasticityMultiplier
//			header.GasLimit = core.CalcGasLimit(parentGasLimit, w.config.GasCeil)
//		}
//	}
//	// Run the consensus preparation with the default or customized consensus engine.
//	if err := w.engine.Prepare(w.chain, header); err != nil {
//		log.Error("Failed to prepare header for sealing", "err", err)
//		return nil, err
//	}
//	// Could potentially happen if starting to mine in an odd state.
//	// Note genParams.coinbase can be different with header.Coinbase
//	// since clique algorithm can modify the coinbase field in header.
//	env, err := w.makeEnv(parent, header, genParams.coinbase, genParams.prevWork)
//	if err != nil {
//		log.Error("Failed to create sealing context", "err", err)
//		return nil, err
//	}
//
//	// Handle upgrade build-in system contract code
//	systemcontracts.UpgradeBuildInSystemContract(w.chainConfig, header.Number, env.state)
//
//	// Accumulate the uncles for the sealing work only if it's allowed.
//	if !genParams.noUncle {
//		commitUncles := func(blocks map[common.Hash]*types.Block) {
//			for hash, uncle := range blocks {
//				if len(env.uncles) == 2 {
//					break
//				}
//				if err := w.commitUncle(env, uncle.Header()); err != nil {
//					log.Trace("Possible uncle rejected", "hash", hash, "reason", err)
//				} else {
//					log.Debug("Committing new uncle to block", "hash", hash)
//				}
//			}
//		}
//		// Prefer to locally generated uncle
//		commitUncles(w.localUncles)
//		commitUncles(w.remoteUncles)
//	}
//	return env, nil
//}

//// commitWork generates several new sealing tasks based on the parent block
//// and submit them to the sealer.
//func (w *worker) commitWork(interruptCh chan int32, timestamp int64) {
//	start := time.Now()
//
//	// Set the coinbase if the worker is running or it's required
//	var coinbase common.Address
//	if w.isRunning() {
//		if w.coinbase == (common.Address{}) {
//			log.Error("Refusing to mine without etherbase")
//			return
//		}
//		coinbase = w.coinbase // Use the preset address as the fee recipient
//	}
//
//	stopTimer := time.NewTimer(0)
//	defer stopTimer.Stop()
//	<-stopTimer.C // discard the initial tick
//
//	stopWaitTimer := time.NewTimer(0)
//	defer stopWaitTimer.Stop()
//	<-stopWaitTimer.C // discard the initial tick
//
//	// validator can try several times to get the most profitable block,
//	// as long as the timestamp is not reached.
//	workList := make([]*environment, 0, 10)
//	var prevWork *environment
//	// workList clean up
//	defer func() {
//		for _, wk := range workList {
//			// only keep the best work, discard others.
//			if wk == w.current {
//				continue
//			}
//			wk.discard()
//		}
//	}()
//
//LOOP:
//	for {
//		work, err := w.prepareWork(&generateParams{
//			timestamp: uint64(timestamp),
//			coinbase:  coinbase,
//			prevWork:  prevWork,
//		})
//		if err != nil {
//			return
//		}
//		prevWork = work
//		workList = append(workList, work)
//
//		delay := w.engine.Delay(w.chain, work.header, &w.config.DelayLeftOver)
//		if delay == nil {
//			log.Warn("commitWork delay is nil, something is wrong")
//			stopTimer = nil
//		} else if *delay <= 0 {
//			log.Debug("Not enough time for commitWork")
//			break
//		} else {
//			log.Debug("commitWork stopTimer", "block", work.header.Number,
//				"header time", time.Until(time.Unix(int64(work.header.Time), 0)),
//				"commit delay", *delay, "DelayLeftOver", w.config.DelayLeftOver)
//			stopTimer.Reset(*delay)
//		}
//
//		// subscribe before fillTransactions
//		txsCh := make(chan core.NewTxsEvent, txChanSize)
//		sub := w.eth.TxPool().SubscribeNewTxsEvent(txsCh)
//		// if TxPool has been stopped, `sub` would be nil, it could happen on shutdown.
//		if sub == nil {
//			log.Info("commitWork SubscribeNewTxsEvent return nil")
//		} else {
//			defer sub.Unsubscribe()
//		}
//
//		// Fill pending transactions from the txpool
//		fillStart := time.Now()
//		err = w.fillTransactions(interruptCh, work, stopTimer)
//		fillDuration := time.Since(fillStart)
//		switch {
//		case errors.Is(err, errBlockInterruptedByNewHead):
//			log.Debug("commitWork abort", "err", err)
//			return
//		case errors.Is(err, errBlockInterruptedByRecommit):
//			fallthrough
//		case errors.Is(err, errBlockInterruptedByTimeout):
//			fallthrough
//		case errors.Is(err, errBlockInterruptedByOutOfGas):
//			// break the loop to get the best work
//			log.Debug("commitWork finish", "reason", err)
//			break LOOP
//		}
//
//		if interruptCh == nil || stopTimer == nil {
//			// it is single commit work, no need to try several time.
//			log.Info("commitWork interruptCh or stopTimer is nil")
//			break
//		}
//
//		newTxsNum := 0
//		// stopTimer was the maximum delay for each fillTransactions
//		// but now it is used to wait until (head.Time - DelayLeftOver) is reached.
//		stopTimer.Reset(time.Until(time.Unix(int64(work.header.Time), 0)) - w.config.DelayLeftOver)
//	LOOP_WAIT:
//		for {
//			select {
//			case <-stopTimer.C:
//				log.Debug("commitWork stopTimer expired")
//				break LOOP
//			case <-interruptCh:
//				log.Debug("commitWork interruptCh closed, new block imported or resubmit triggered")
//				return
//			case ev := <-txsCh:
//				delay := w.engine.Delay(w.chain, work.header, &w.config.DelayLeftOver)
//				log.Debug("commitWork txsCh arrived", "fillDuration", fillDuration.String(),
//					"delay", delay.String(), "work.tcount", work.tcount,
//					"newTxsNum", newTxsNum, "len(ev.Txs)", len(ev.Txs))
//				if *delay < fillDuration {
//					// There may not have enough time for another fillTransactions.
//					break LOOP
//				} else if *delay < fillDuration*2 {
//					// We can schedule another fillTransactions, but the time is limited,
//					// probably it is the last chance, schedule it immediately.
//					break LOOP_WAIT
//				} else {
//					// There is still plenty of time left.
//					// We can wait a while to collect more transactions before
//					// schedule another fillTransaction to reduce CPU cost.
//					// There will be 2 cases to schedule another fillTransactions:
//					//   1.newTxsNum >= work.tcount
//					//   2.no much time left, have to schedule it immediately.
//					newTxsNum = newTxsNum + len(ev.Txs)
//					if newTxsNum >= work.tcount {
//						break LOOP_WAIT
//					}
//					stopWaitTimer.Reset(*delay - fillDuration*2)
//				}
//			case <-stopWaitTimer.C:
//				if newTxsNum > 0 {
//					break LOOP_WAIT
//				}
//			}
//		}
//		// if sub's channel if full, it will block other NewTxsEvent subscribers,
//		// so unsubscribe ASAP and Unsubscribe() is re-enterable, safe to call several time.
//		if sub != nil {
//			sub.Unsubscribe()
//		}
//	}
//	// get the most profitable work
//	bestWork := workList[0]
//	bestReward := new(big.Int)
//	for i, wk := range workList {
//		balance := wk.state.GetBalance(consensus.SystemAddress)
//		log.Debug("Get the most profitable work", "index", i, "balance", balance, "bestReward", bestReward)
//		if balance.Cmp(bestReward) > 0 {
//			bestWork = wk
//			bestReward = balance
//		}
//	}
//	w.commit(bestWork, w.fullTaskHook, true, start)
//
//	// Swap out the old work with the new one, terminating any leftover
//	// prefetcher processes in the mean time and starting a new one.
//	if w.current != nil {
//		w.current.discard()
//	}
//	w.current = bestWork
//}

// commit runs any post-transaction state modifications, assembles the final block
// and commits new work if consensus engine is running.
// Note the assumption is held that the mutation is allowed to the passed env, do
// the deep copy first.
func (w *worker) commit(env *core.MinerEnvironment, interval func(), update bool, start time.Time) error {
	if w.isRunning() {
		if interval != nil {
			interval()
		}

		err := env.State.WaitPipeVerification()
		if err != nil {
			return err
		}
		env.State.CorrectAccountsRoot(w.chain.CurrentBlock().Root())

		finalizeStart := time.Now()
		block, receipts, err := w.engine.FinalizeAndAssemble(w.chain, types.CopyHeader(env.Header), env.State, env.PackedTxs, nil, env.Receipts)
		if err != nil {
			return err
		}
		finalizeBlockTimer.UpdateSince(finalizeStart)

		// Create a local environment copy, avoid the data race with snapshot state.
		// https://github.com/ethereum/go-ethereum/issues/24299
		env := env.Copy()

		// If we're post merge, just ignore
		if !w.isTTDReached(block.Header()) {
			select {
			case w.taskCh <- &task{receipts: receipts, state: env.State, block: block, createdAt: time.Now()}:
				w.unconfirmed.Shift(block.NumberU64() - 1)
				log.Info("Commit new mining work", "number", block.Number(), "sealhash", w.engine.SealHash(block.Header()),
					"txs", env.TxCount,
					"gas", block.GasUsed(),
					"elapsed", common.PrettyDuration(time.Since(start)))

			case <-w.exitCh:
				log.Info("Worker has exited")
			}
		}
	}
	if update {
		w.updateSnapshot(env)
	}
	return nil
}

// getSealingBlock generates the sealing block based on the given parameters.
func (w *worker) getSealingBlock(parent common.Hash, timestamp uint64, coinbase common.Address, random common.Hash) (*types.Block, error) {
	req := &getWorkReq{
		params: &generateParams{
			timestamp:  timestamp,
			forceTime:  true,
			parentHash: parent,
			coinbase:   coinbase,
			random:     random,
			noUncle:    true,
			noExtra:    true,
		},
		result: make(chan *types.Block, 1),
	}
	select {
	case w.getWorkCh <- req:
		block := <-req.result
		if block == nil {
			return nil, req.err
		}
		return block, nil
	case <-w.exitCh:
		return nil, errors.New("miner closed")
	}
}

// isTTDReached returns the indicator if the given block has reached the total
// terminal difficulty for The Merge transition.
func (w *worker) isTTDReached(header *types.Header) bool {
	td, ttd := w.chain.GetTd(header.ParentHash, header.Number.Uint64()-1), w.chain.Config().TerminalTotalDifficulty
	return td != nil && ttd != nil && td.Cmp(ttd) >= 0
}

// postSideBlock fires a side chain event, only use it for testing.
func (w *worker) postSideBlock(event core.ChainSideEvent) {
	select {
	case w.chainSideCh <- event:
	case <-w.exitCh:
	}
}

// signalToErr converts the interruption signal to a concrete error type for return.
// The given signal must be a valid interruption signal.
func signalToErr(signal int32) error {
	switch signal {
	case commitInterruptNone:
		return nil
	case commitInterruptNewHead:
		return errBlockInterruptedByNewHead
	case commitInterruptResubmit:
		return errBlockInterruptedByRecommit
	case commitInterruptTimeout:
		return errBlockInterruptedByTimeout
	case commitInterruptOutOfGas:
		return errBlockInterruptedByOutOfGas
	default:
		panic(fmt.Errorf("undefined signal %d", signal))
	}
}
