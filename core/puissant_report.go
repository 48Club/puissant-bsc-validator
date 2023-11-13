package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	mapset "github.com/deckarep/golang-set/v2"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"math/big"
	"net/http"
	"strconv"
	"time"
)

type puissantStatusCode uint8
type puissantInfoCode uint8
type puissantTxStatusCode uint8

type tUploadData struct {
	BlockNumber string             `json:"block"`
	Result      []*tUploadPuissant `json:"result"`
}

type tUploadPuissant struct {
	UUID   string                `json:"uuid"`
	Status puissantStatusCode    `json:"status"`
	Info   puissantInfoCode      `json:"info"`
	Txs    []*tUploadTransaction `json:"txs"`
}

type tUploadTransaction struct {
	TxHash    string               `json:"tx_hash"`
	GasUsed   uint64               `json:"gas_used"`
	Status    puissantTxStatusCode `json:"status"`
	RevertMsg string               `json:"revert_msg"`
}

const (
	PuissantStatusWellDone puissantStatusCode = 0
	PuissantStatusPending  puissantStatusCode = 1
	PuissantStatusDropped  puissantStatusCode = 2

	PuissantInfoCodeOk             puissantInfoCode = 0
	PuissantInfoCodeExpired        puissantInfoCode = 1
	PuissantInfoCodeInvalidPayment puissantInfoCode = 2
	PuissantInfoCodeRevert         puissantInfoCode = 3
	PuissantInfoCodeBeaten         puissantInfoCode = 4

	PuissantTransactionStatusOk               puissantTxStatusCode = 0
	PuissantTransactionStatusRevert           puissantTxStatusCode = 1
	PuissantTransactionStatusPreCheckFailed   puissantTxStatusCode = 2
	PuissantTransactionStatusConflictedBeaten puissantTxStatusCode = 3
	PuissantTransactionStatusNoRun            puissantTxStatusCode = 4
	PuissantTransactionStatusInvalidPayment   puissantTxStatusCode = 5
)

func (code puissantStatusCode) ToIcon() string {
	switch code {
	case PuissantStatusWellDone:
		return "✅"
	case PuissantStatusDropped:
		return "❌"
	default:
		return "❓"
	}
}

func (code puissantTxStatusCode) ToString() string {
	switch code {
	case PuissantTransactionStatusOk:
		return "OK"
	case PuissantTransactionStatusRevert:
		return "Reverted"
	case PuissantTransactionStatusConflictedBeaten:
		return "Conflicted"
	case PuissantTransactionStatusNoRun:
		return "NoRun"
	case PuissantTransactionStatusInvalidPayment:
		return "InvalidPayment"
	case PuissantTransactionStatusPreCheckFailed:
		return "PreCheckFailed"
	default:
		return "Unknown"
	}
}

type CommitterReportList []*CommitterReport

func (p CommitterReportList) Len() int {
	return len(p)
}

func (p CommitterReportList) Less(i, j int) bool {
	return p[i].Txs[0].gasPrice.Cmp(p[j].Txs[0].gasPrice) > 0
}

func (p CommitterReportList) Swap(i, j int) {
	p[i], p[j] = p[j], p[i]
}

type puissantReporter struct {
	data     map[int]CommitterReportList
	evmTried mapset.Set[types.PuissantID]
	loaded   mapset.Set[types.PuissantID]

	client *http.Client
}

func NewPuissantReporter() *puissantReporter {
	return &puissantReporter{
		data:     make(map[int]CommitterReportList),
		evmTried: mapset.NewThreadUnsafeSet[types.PuissantID](),
		loaded:   mapset.NewThreadUnsafeSet[types.PuissantID](),

		client: &http.Client{Timeout: 5 * time.Second},
	}
}

func (pr *puissantReporter) Update(newGroup CommitterReportList, round int) {
	if newGroup == nil {
		return
	}

	pr.data[round] = newGroup

	for _, update := range newGroup {
		if update.EvmRun {
			pr.evmTried.Add(update.PuissantID)
		}
		pr.loaded.Add(update.PuissantID)
	}
}

func (pr *puissantReporter) Done(bestRound int, blockNumber uint64, blockIncome *big.Int, senderFn func(text string, mute bool), msgSigner func(text []byte) []byte) (ret []common.Hash) {

	if pr.loaded.Cardinality() == 0 {
		return nil
	}

	var (
		start        = time.Now()
		success      int
		puiIncomeF   float64
		incomeS      string
		blockIncomeF = types.WeiToEther(blockIncome)
		text         = fmt.Sprintf("[%d](https://bscscan.com/block/%d)\n\n", blockNumber, blockNumber)
	)
	puissantList, ok := pr.data[bestRound]
	if !ok {
		return nil
	}

	for index, each := range puissantList {
		incomeF := types.WeiToEther(each.Income)
		text += fmt.Sprintf("*Rank: %d, %.3fbnb*\n", index+1, incomeF)
		for txSeq, tx := range each.Txs {
			if txSeq == 0 {
				text += fmt.Sprintf(" [TX-%d](%s): used=%d, *%dgw*\n", txSeq+1, "https://bscscan.com/tx/"+tx.hash.Hex(), tx.gasUsed, new(big.Int).Div(tx.gasPrice, big.NewInt(params.GWei)).Uint64())
			} else {
				text += fmt.Sprintf(" [TX-%d](%s): used=%d\n", txSeq+1, "https://bscscan.com/tx/"+tx.hash.Hex(), tx.gasUsed)
			}
			ret = append(ret, tx.hash)
		}
		success++
		puiIncomeF += incomeF
		text += "\n"
	}

	if puiIncomeF > 0 && blockIncomeF > 0 {
		incomeS = fmt.Sprintf("%.3f/%.3f (%d%%)", puiIncomeF, blockIncomeF, int(puiIncomeF*100/blockIncomeF))
	} else {
		incomeS = fmt.Sprintf("%.3f", blockIncomeF)
	}
	text += fmt.Sprintf("*⏱️round: %d (%d)*\n*🧾puissant: %d / %d / %d*\n*💰income: %s*\n", bestRound+1, len(pr.data)+1, success, pr.evmTried.Cardinality(), pr.loaded.Cardinality(), incomeS)

	senderFn(text, true)
	if msgSigner != nil {
		pr.send(start, bestRound, blockNumber, msgSigner)
	}
	return ret
}

func (pr *puissantReporter) send(start time.Time, bestRound int, blockNumber uint64, msgSigner func(text []byte) []byte) {
	if len(pr.data) == 0 {
		return
	}
	var (
		body = tUploadData{BlockNumber: hexutil.EncodeUint64(blockNumber)}
		tmp  = make(map[types.PuissantID]*tUploadPuissant)
	)

	for round := 0; round <= bestRound; round++ {
		if roundData, ok := pr.data[round]; ok {
			for _, detail := range roundData {
				if round == bestRound || detail.Status != PuissantStatusWellDone {
					each := &tUploadPuissant{UUID: detail.PuissantID.Hex(), Status: detail.Status, Info: detail.Info, Txs: make([]*tUploadTransaction, len(detail.Txs))}
					for txSeq, tx := range detail.Txs {
						each.Txs[txSeq] = &tUploadTransaction{
							TxHash:    tx.hash.Hex(),
							GasUsed:   tx.gasUsed,
							Status:    tx.status,
							RevertMsg: tx.revertMsg,
						}
					}
					tmp[detail.PuissantID] = each
				}
			}
		}
	}
	for _, detail := range tmp {
		body.Result = append(body.Result, detail)
	}

	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	b, _ := json.Marshal(body)

	req, err := http.NewRequest(http.MethodPost, types.PuissantStatusReportURL, bytes.NewBuffer(b))
	if err != nil {
		panic(err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("timestamp", timestamp)
	req.Header.Set("sign", hexutil.Encode(msgSigner([]byte(timestamp))))

	resp, err := pr.client.Do(req)
	if err != nil {
		log.Error("❌ report packing result failed", "err", err, "elapsed", time.Since(start))
	} else if resp.StatusCode != http.StatusOK {
		log.Error("❌ report packing result failed", "StatusCode", resp.StatusCode, "elapsed", time.Since(start))
	}
	_ = resp.Body.Close()
}
