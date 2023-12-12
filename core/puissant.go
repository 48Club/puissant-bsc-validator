package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	mapset "github.com/deckarep/golang-set/v2"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
)

type puissantRuntime struct {
	env            *MinerEnvironment
	snapshots      *snapshotSet
	chain          *BlockChain
	chainConfig    *params.ChainConfig
	bloomProcessor *ReceiptBloomGenerator

	bundles types.PuissantBundles

	resultOK     mapset.Set[types.PuissantID]
	resultFailed mapset.Set[types.PuissantID]
}

func RunPuissantCommitter(
	ctx context.Context,
	round int,
	interruptCh chan int32,
	signalToErr func(int32) error,

	workerEnv *MinerEnvironment,
	pendingPool map[common.Address][]*types.Transaction,
	puissantPool types.PuissantBundles,
	chain *BlockChain,
	chainConfig *params.ChainConfig,
	poolRemoveFn func(mapset.Set[types.PuissantID]),
) (bool, error) {

	var committer = &puissantRuntime{
		env:            workerEnv,
		snapshots:      newSnapshotSet(),
		chain:          chain,
		chainConfig:    chainConfig,
		bloomProcessor: NewReceiptBloomGenerator(),

		bundles:      puissantPool,
		resultOK:     mapset.NewThreadUnsafeSet[types.PuissantID](),
		resultFailed: mapset.NewThreadUnsafeSet[types.PuissantID](),
	}
	defer poolRemoveFn(committer.resultFailed)

	puissantPool.PreparePacking(workerEnv.Header.Number.Uint64(), round)

	workerEnv.PuissantTxQueue = types.NewTransactionsPuissant(workerEnv.Signer, pendingPool, puissantPool)
	committer.resetPackingBundles(round)

	for {
		select {
		case <-ctx.Done():
			// timeout
			return false, committer.finalIntegrityCheck()

		case signal := <-interruptCh:
			log.Info("packing abort due to interruption", "when", "packingRunTime", "reason", signalToErr(signal))
			return true, signalToErr(signal)

		default:
		}

		if gasLeft := workerEnv.GasPool.Gas(); gasLeft < params.TxGas {
			log.Warn(" 🐶 not enough gas for further transactions, commit directly", "packed-count", workerEnv.TxCount, "have", gasLeft, "want", params.TxGas)
			return false, committer.finalIntegrityCheck()
		}

		tx := workerEnv.PuissantTxQueue.Peek()
		if tx == nil {
			return false, nil
		}

		committer.commitTransaction(round, tx)
	}
}

func (p *puissantRuntime) finalIntegrityCheck() error {
	if p.bundles.IsEmpty() {
		return nil
	}

	var (
		includedBundle = mapset.NewThreadUnsafeSet[types.PuissantID]()
		shouldIncluded int
		included       int
	)

	for _, tx := range p.env.PackedTxs {
		if bundle, _ := tx.Bundle(); bundle != nil {
			if includedBundle.Add(bundle.ID()) {
				shouldIncluded += bundle.TxCount()
			}
			included++
		}
	}
	if shouldIncluded != included {
		return fmt.Errorf("unfinished bundle included, has=%d, want=%d", included, shouldIncluded)
	}
	return nil
}

func (p *puissantRuntime) commitTransaction(round int, tx *types.Transaction) {
	var (
		theBundle, bundleTxSeq = tx.Bundle()
		isBundleTx             = theBundle != nil
	)

	if isBundleTx {
		p.snapshots.save(theBundle.ID(), p.env)
	}

	p.env.State.SetTxContext(tx.Hash(), p.env.TxCount)

	txReceipt, err := commitTransaction(tx, p.chain, p.chainConfig, p.env.Header.Coinbase,
		p.env.State, p.env.GasPool, p.env.Header,
		isBundleTx && !theBundle.Revertible(tx.Hash()), isBundleTx && bundleTxSeq == 0, p.bloomProcessor)

	if err == nil {
		p.env.PackTx(tx, txReceipt)
	}

	// If it's not a puissant tx, return and run next. Otherwise, update puissant status
	if !isBundleTx {
		if errors.Is(err, ErrGasLimitReached) ||
			errors.Is(err, ErrNonceTooHigh) ||
			errors.Is(err, ErrTxTypeNotSupported) {
			p.env.PuissantTxQueue.Pop()
		} else {
			p.env.PuissantTxQueue.Shift()
		}

		return
	}

	if err != nil {
		//log.Warn("puissant-tx-failed", "seq", pSeq, "tx", bundleTxSeq, "err", err)

		theBundle.UpdateTransactionStatus(round, tx.Hash(), 0, types.PuiTransactionStatusRevert, err)

		p.resultFailed.Add(theBundle.ID())

		p.snapshots.revert(theBundle.ID(), p.env)
		// revert first, then reset
		p.resetPackingBundles(round)

	} else {
		//log.Info("puissant-tx-pass", "seq", pSeq, "tx", bundleTxSeq)

		theBundle.UpdateTransactionStatus(round, tx.Hash(), txReceipt.GasUsed, types.PuiTransactionStatusOk, nil)

		if theBundle.IsPosted(round) {
			p.resultOK.Add(theBundle.ID())
		}
		p.env.PuissantTxQueue.Pop()
	}
}

func (p *puissantRuntime) resetPackingBundles(round int) {
	var (
		included = mapset.NewThreadUnsafeSet[string]()
		enabled  []types.PuissantID
	)

	for _, bundle := range p.bundles {
		var conflict bool

		if p.resultFailed.Contains(bundle.ID()) {
			continue
		}

		for _, tx := range bundle.Txs() {
			if included.Contains(txUniqueID(p.env.Signer, tx)) {
				conflict = true
				bundle.UpdateTransactionStatus(
					round,
					tx.Hash(),
					0,
					types.PuiTransactionStatusConflicted,
					types.PuiTransactionStatusConflicted.ToError(),
				)
				break
			}
		}
		if !conflict {
			for _, tx := range bundle.Txs() {
				included.Add(txUniqueID(p.env.Signer, tx))
			}
			enabled = append(enabled, bundle.ID())
		}
	}
	p.env.PuissantTxQueue.ResetEnable(enabled)
}

func txUniqueID(signer types.Signer, tx *types.Transaction) string {
	var (
		id        bytes.Buffer
		nonce     = tx.Nonce()
		sender, _ = types.Sender(signer, tx)
	)

	id.Grow(common.AddressLength + 4)
	id.Write(sender.Bytes())
	id.WriteByte(byte(nonce))
	id.WriteByte(byte(nonce >> 8))
	id.WriteByte(byte(nonce >> 16))
	id.WriteByte(byte(nonce >> 24))

	return id.String()
}

type envData struct {
	storeEnv   *MinerEnvironment
	snapshotID int
}

type snapshotSet struct {
	snapshotID int
	snapshots  map[types.PuissantID]*envData
}

func newSnapshotSet() *snapshotSet {
	return &snapshotSet{snapshots: make(map[types.PuissantID]*envData)}
}

func (pc *snapshotSet) save(pid types.PuissantID, currEnv *MinerEnvironment) bool {
	if _, ok := pc.snapshots[pid]; ok {
		return false
	}
	pc.snapshots[pid] = &envData{
		storeEnv:   currEnv.Copy(),
		snapshotID: pc.snapshotID,
	}
	//log.Info(" 🐶 📷 take snapshot", "snapshotID", pc.snapshotID, "packed-count", currEnv.TxCount)
	pc.snapshotID++
	return true
}

func (pc *snapshotSet) revert(bID types.PuissantID, currEnv *MinerEnvironment) {
	load := pc.snapshots[bID]

	if currEnv.TxCount != load.storeEnv.TxCount {
		// should reload work env
		//load.storeEnv.State.TransferPrefetcher(currEnv.State)
		*currEnv = *load.storeEnv
	}

	for _bID, data := range pc.snapshots {
		if data.snapshotID >= load.snapshotID {
			delete(pc.snapshots, _bID)
			//log.Warn(" 🐶 remove state-snapshot", "snapshotID", data.snapshotID)
		}
	}
}
