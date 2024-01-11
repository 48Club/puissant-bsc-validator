package types

import (
	mapset "github.com/deckarep/golang-set/v2"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
	"math/big"
	"sort"
)

type PuissantBundle struct {
	id         PuissantID
	txs        Transactions
	expireAt   uint64
	revertible mapset.Set[common.Hash]
	bidPrice   *big.Int // gas price of the first transaction

	lastTriedBlock uint64

	status map[int]*PuiBundleStatus
}

func NewPuissantBundle(txs Transactions, revertible mapset.Set[common.Hash], maxTS uint64) *PuissantBundle {
	if txs.Len() == 0 {
		panic("empty bundle")
	}

	theBundle := &PuissantBundle{
		id:         genPuissantID(txs, revertible, maxTS),
		txs:        txs,
		expireAt:   maxTS,
		revertible: revertible,
		bidPrice:   txs[0].GasTipCap(),
	}
	for index, tx := range txs {
		tx.SetBundle(theBundle, index)
	}
	return theBundle
}

func (pp *PuissantBundle) PreparePacking(blockNumber uint64, round int) {
	if blockNumber != pp.lastTriedBlock {
		pp.status = make(map[int]*PuiBundleStatus)
		pp.lastTriedBlock = blockNumber
	}
	var txs = make([]*PuiTransactionStatus, len(pp.txs))
	for index := range pp.txs {
		txs[index] = &PuiTransactionStatus{Hash: pp.txs[index].Hash(), Error: PuiErrTxNoRun}
	}
	pp.status[round] = &PuiBundleStatus{Fee: new(big.Int), Txs: txs}
}

func (pp *PuissantBundle) UpdateTransactionStatus(round int, hash common.Hash, gasUsed uint64, status PuiTransactionStatusCode, err error) {
	bundle := pp.status[round]

	for i, tx := range pp.txs {
		if tx.Hash() == hash {
			bundle.Txs[i].Status = status
			bundle.Txs[i].GasUsed = gasUsed
			bundle.Txs[i].Error = err
			break
		}
	}
}

func (pp *PuissantBundle) RoundStatus(round int) *PuiBundleStatus {
	return pp.status[round]
}

func (pp *PuissantBundle) IsPosted(round int) bool {
	bundle := pp.status[round]

	for _, tx := range bundle.Txs {
		if tx.Status != PuiTransactionStatusOk {
			return false
		}
	}
	return true
}

func (pp *PuissantBundle) FinalizeRound(round int) (*PuiBundleStatus, bool) {
	bStatus := pp.RoundStatus(round)
	if bStatus == nil {
		return nil, false
	}
	if bStatus.Status != PuiBundleStatusNoRun {
		return bStatus, bStatus.Status == PuiBundleStatusOK
	}

	var failed bool
	for index := pp.txs.Len() - 1; index >= 0; index-- {
		if txStatus := bStatus.Txs[index].Status; txStatus.IsFailed() {
			failed = true
			if uint8(txStatus) > uint8(bStatus.Status) {
				bStatus.Status = PuiBundleStatusCode(txStatus)
				bStatus.Error = pp.txs[index].Errorf(bStatus.Txs[index].Error)
			}
		}
	}
	if !failed {
		bStatus.Status = PuiBundleStatusOK
		bStatus.Fee = new(big.Int).Mul(pp.txs[0].GasTipCap(), new(big.Int).SetUint64(bStatus.Txs[0].GasUsed))
	}

	return bStatus, !failed
}

func (pp *PuissantBundle) Revertible(hash common.Hash) bool {
	return pp.revertible.Contains(hash)
}

func (pp *PuissantBundle) ID() PuissantID {
	return pp.id
}

func (pp *PuissantBundle) Sender(signer Signer) (common.Address, error) {
	return Sender(signer, pp.txs[0])
}

func (pp *PuissantBundle) ExpireAt() uint64 {
	return pp.expireAt
}

func (pp *PuissantBundle) Txs() Transactions {
	return pp.txs
}

func (pp *PuissantBundle) TxCount() int {
	return len(pp.txs)
}

func (pp *PuissantBundle) HasHigherBidPriceThan(with *PuissantBundle) bool {
	return pp.bidPrice.Cmp(with.bidPrice) > 0
}

func (pp *PuissantBundle) HasHigherBidPriceIntCmp(with *big.Int) bool {
	return pp.bidPrice.Cmp(with) > 0
}

func (pp *PuissantBundle) ReplacedByNewPuissant(np *PuissantBundle, priceBump uint64) bool {
	oldP := new(big.Int).Mul(pp.bidPrice, big.NewInt(100+int64(priceBump)))
	newP := new(big.Int).Mul(np.bidPrice, big.NewInt(100))

	return newP.Cmp(oldP) > 0
}

func (pp *PuissantBundle) BidPrice() *big.Int {
	return new(big.Int).Set(pp.bidPrice)
}

// PuissantBundles list of PuissantBundle
type PuissantBundles []*PuissantBundle

func (p PuissantBundles) Len() int {
	return len(p)
}

func (p PuissantBundles) Less(i, j int) bool {
	return p[i].HasHigherBidPriceThan(p[j])
}

func (p PuissantBundles) Swap(i, j int) {
	p[i], p[j] = p[j], p[i]
}

func (p PuissantBundles) PreparePacking(blockNumber uint64, round int) {
	for _, bundle := range p {
		bundle.PreparePacking(blockNumber, round)
	}
}

func (p PuissantBundles) IsEmpty() bool {
	return len(p) == 0
}

// transactions queue
type puissantTxQueue Transactions

func (s puissantTxQueue) Len() int { return len(s) }
func (s puissantTxQueue) Less(i, j int) bool {
	bundleSort := func(txI, txJ *Transaction) bool {
		if txI.bundle.ID() == txJ.bundle.ID() {
			return txI.bundleTxIndex < txJ.bundleTxIndex
		}
		return txI.bundle.HasHigherBidPriceThan(txJ.bundle)
	}

	cmp := s[i].GasTipCap().Cmp(s[j].GasTipCap())
	if cmp == 0 {
		iIsBundle := s[i].bundle != nil
		jIsBundle := s[j].bundle != nil

		if !iIsBundle && !jIsBundle {
			return s[i].Time().Before(s[j].Time())

		} else if iIsBundle && jIsBundle {
			return bundleSort(s[i], s[j])

		} else if iIsBundle {
			return true

		} else if jIsBundle {
			return false

		}
	}
	return cmp > 0
}

func (s puissantTxQueue) Swap(i, j int) { s[i], s[j] = s[j], s[i] }

type TransactionsPuissant struct {
	txs                map[common.Address][]*Transaction
	txHeadsAndPuissant puissantTxQueue
	signer             Signer
	enabled            mapset.Set[PuissantID]
}

func NewTransactionsPuissant(signer Signer, txs map[common.Address][]*Transaction, bundles PuissantBundles) *TransactionsPuissant {
	headsAndBundleTxs := make(puissantTxQueue, 0, len(txs))
	for from, accTxs := range txs {
		// Ensure the sender address is from the signer
		if acc, _ := Sender(signer, accTxs[0]); acc != from {
			delete(txs, from)
			continue
		}
		headsAndBundleTxs = append(headsAndBundleTxs, accTxs[0])
		txs[from] = accTxs[1:]
	}

	for _, each := range bundles {
		for _, tx := range each.Txs() {
			headsAndBundleTxs = append(headsAndBundleTxs, tx)
		}
	}

	sort.Sort(&headsAndBundleTxs)

	for _, tx := range headsAndBundleTxs {
		if bundle, txSeq := tx.Bundle(); bundle != nil {
			log.Info("ptx-queue", "hash", tx.Hash().String(), "bid", bundle.ID().String(), "txSeq", txSeq, "gp", tx.GasTipCap())
		}
	}

	return &TransactionsPuissant{
		enabled:            mapset.NewThreadUnsafeSet[PuissantID](),
		txs:                txs,
		txHeadsAndPuissant: headsAndBundleTxs,
		signer:             signer,
	}
}

func (t *TransactionsPuissant) ResetEnable(pids []PuissantID) {
	t.enabled.Clear()
	for _, pid := range pids {
		t.enabled.Add(pid)
	}
}

func (t *TransactionsPuissant) Copy() *TransactionsPuissant {
	if t == nil {
		return nil
	}

	newHeadsAndBundleTxs := make([]*Transaction, len(t.txHeadsAndPuissant))
	copy(newHeadsAndBundleTxs, t.txHeadsAndPuissant)
	txs := make(map[common.Address][]*Transaction, len(t.txs))
	for acc, txsTmp := range t.txs {
		txs[acc] = txsTmp
	}
	return &TransactionsPuissant{txHeadsAndPuissant: newHeadsAndBundleTxs, txs: txs, signer: t.signer, enabled: t.enabled.Clone()}
}

func (t *TransactionsPuissant) Peek() *Transaction {
	if len(t.txHeadsAndPuissant) == 0 {
		return nil
	}
	next := t.txHeadsAndPuissant[0]
	if next.bundle != nil && !t.enabled.Contains(next.bundle.ID()) {
		t.Pop()
		return t.Peek()
	}
	return next
}

func (t *TransactionsPuissant) Shift() {
	acc, _ := Sender(t.signer, t.txHeadsAndPuissant[0])
	if t.txHeadsAndPuissant[0].bundle == nil {
		if txs, ok := t.txs[acc]; ok && len(txs) > 0 {
			t.txHeadsAndPuissant[0], t.txs[acc] = txs[0], txs[1:]
			sort.Sort(&t.txHeadsAndPuissant)
			return
		}
	}
	t.Pop()
}

// Pop removes the best transaction, *not* replacing it with the next one from
// the same account. This should be used when a transaction cannot be executed
// and hence all subsequent ones should be discarded from the same account.
func (t *TransactionsPuissant) Pop() {
	if len(t.txHeadsAndPuissant) > 0 {
		t.txHeadsAndPuissant = t.txHeadsAndPuissant[1:]
	}
}

func WeiToEther(wei *big.Int) float64 {
	if wei == nil {
		return 0
	}
	// 1 ether = 10^18 wei
	ether := new(big.Float).SetInt(big.NewInt(0).Exp(big.NewInt(10), big.NewInt(18), nil))

	// Convert wei to big.Float
	weiFloat := new(big.Float).SetInt(wei)

	// Divide wei by ether to get the amount in ethers
	ethValue := new(big.Float).Quo(weiFloat, ether)

	f, _ := ethValue.Float64()
	return f
}
