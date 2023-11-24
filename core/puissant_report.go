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
	"strings"
	"time"
)

var defaultClient = &http.Client{Timeout: 5 * time.Second}

type PuissantReporter struct {
	bundleRecords map[int]types.PuissantBundles
	evmTried      mapset.Set[types.PuissantID]
	loaded        mapset.Set[types.PuissantID]
	maxRound      int

	reportURL string
}

func NewPuissantReporter(reportURL string) *PuissantReporter {
	return &PuissantReporter{
		bundleRecords: make(map[int]types.PuissantBundles),
		evmTried:      mapset.NewThreadUnsafeSet[types.PuissantID](),
		loaded:        mapset.NewThreadUnsafeSet[types.PuissantID](),

		reportURL: reportURL,
	}
}

func (pr *PuissantReporter) FinalizeRound(round int, finishedBundles types.PuissantBundles) {
	pr.bundleRecords[round] = finishedBundles
	pr.maxRound = round

	for _, bundle := range finishedBundles {
		if bundle.RoundStatus(round).Txs[0].Status != types.PuiTransactionStatusNoRun {
			pr.evmTried.Add(bundle.ID())
		}
		pr.loaded.Add(bundle.ID())
	}
}

func (pr *PuissantReporter) Finalize(blockNumber uint64, bestRound int, blockIncome *big.Int, msgSigner func([]byte) ([]byte, error)) string {
	if pr.loaded.Cardinality() == 0 {
		return ""
	}

	var (
		telegram = buildTelegramReporter(blockNumber)

		resultOK     = make(map[types.PuissantID]*types.PuiBundleStatus)
		resultFailed = make(map[types.PuissantID]*types.PuiBundleStatus)
	)

	// first, add all success bundles from bestRound
	for _, bundle := range pr.bundleRecords[bestRound] {
		if bundleRound, ok := bundle.FinalizeRound(bestRound); ok {
			resultOK[bundle.ID()] = bundleRound
			telegram.Add(bundle, bundleRound)
		}
	}

	// then, add all failed bundle from all rounds
	for round := 0; round < pr.maxRound; round++ {
		roundRecords, ok := pr.bundleRecords[round]
		if !ok {
			continue
		}

		for _, bundle := range roundRecords {
			if _, exist := resultOK[bundle.ID()]; exist {
				continue
			}
			bundleRound, success := bundle.FinalizeRound(round)
			if success {
				continue
			}
			resultFailed[bundle.ID()] = bundleRound
		}
	}

	if len(pr.reportURL) > 0 && msgSigner != nil {
		go pr.reportToAPI(resultOK, resultFailed, blockNumber, msgSigner)
	}

	return telegram.Finish(blockIncome, bestRound, pr.maxRound, pr.evmTried.Cardinality(), pr.loaded.Cardinality())
}

func (pr *PuissantReporter) reportToAPI(resultOK, resultFailed map[types.PuissantID]*types.PuiBundleStatus, blockNumber uint64, msgSigner func([]byte) ([]byte, error)) {
	var (
		start     = time.Now()
		data      = make([]tUploadBundleResult, 0, len(resultOK)+len(resultFailed))
		bnStr     = strconv.FormatUint(blockNumber, 10)
		sign, err = msgSigner([]byte(bnStr))
	)
	if err != nil {
		log.Error("sign report message failed", "err", err)
		return
	}

	errToStr := func(err error) string {
		if err == nil {
			return ""
		}
		return err.Error()
	}

	resultAddUp := func(result map[types.PuissantID]*types.PuiBundleStatus) {
		for pid, bundle := range result {
			each := tUploadBundleResult{
				PuissantID: pid.Hex(),
				Status:     bundle.Status,
				ErrMsg:     errToStr(bundle.Error),
				Txs:        make([]*tUploadTransactionResult, 0, len(bundle.Txs)),
			}
			for _, tx := range bundle.Txs {
				each.Txs = append(each.Txs, &tUploadTransactionResult{
					TxHash:  tx.Hash.Hex(),
					GasUsed: tx.GasUsed,
					Status:  tx.Status,
					ErrMsg:  errToStr(tx.Error),
				})
			}
			data = append(data, each)
		}
	}

	resultAddUp(resultOK)
	resultAddUp(resultFailed)

	b, _ := json.Marshal(data)
	req, _ := http.NewRequest(http.MethodPost, pr.reportURL, bytes.NewBuffer(b))

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("blockNumber", bnStr)
	req.Header.Set("sign", hexutil.Encode(sign))

	if resp, err := defaultClient.Do(req); err != nil {
		log.Error("report puissant block failed", "err", err, "elapsed", time.Since(start))
	} else {
		_ = resp.Body.Close()
	}
}

type tUploadBundleResult struct {
	PuissantID string                      `json:"pid"`
	Status     types.PuiBundleStatusCode   `json:"status"`
	ErrMsg     string                      `json:"err"`
	Txs        []*tUploadTransactionResult `json:"txs"`
}

type tUploadTransactionResult struct {
	TxHash  string                         `json:"hash"`
	GasUsed uint64                         `json:"gas"`
	ErrMsg  string                         `json:"err"`
	Status  types.PuiTransactionStatusCode `json:"status"`
}

type PuissantTelegramReporter struct {
	content   strings.Builder
	success   int
	puiIncome *big.Int
}

func buildTelegramReporter(blockNumber uint64) *PuissantTelegramReporter {
	tgr := &PuissantTelegramReporter{puiIncome: big.NewInt(0)}
	tgr.content.WriteString(fmt.Sprintf("[%d](https://bscscan.com/block/%d)\n\n", blockNumber, blockNumber))
	return tgr
}

func (ptg *PuissantTelegramReporter) Add(bundle *types.PuissantBundle, roundStatus *types.PuiBundleStatus) {
	ptg.success++

	ptg.content.WriteString(fmt.Sprintf("*Bundle %.3fbnb*\n", types.WeiToEther(roundStatus.Fee)))

	for txSeq, tx := range bundle.Txs() {
		if txSeq == 0 {
			ptg.content.WriteString(fmt.Sprintf(" [TX-%d](%s): used=%d, *%dgw*\n", txSeq+1, "https://bscscan.com/tx/"+tx.Hash().Hex(), roundStatus.Txs[txSeq].GasUsed, new(big.Int).Div(tx.GasTipCap(), big.NewInt(params.GWei)).Uint64()))
		} else {
			ptg.content.WriteString(fmt.Sprintf(" [TX-%d](%s): used=%d\n", txSeq+1, "https://bscscan.com/tx/"+tx.Hash().Hex(), roundStatus.Txs[txSeq].GasUsed))
		}
	}
	ptg.puiIncome.Add(ptg.puiIncome, roundStatus.Fee)
	ptg.content.WriteString("\n")
}

func (ptg *PuissantTelegramReporter) Finish(blockIncome *big.Int, bestRound, totalRound, tried, total int) string {
	blockIncomeF := types.WeiToEther(blockIncome)
	puiIncomeF := types.WeiToEther(ptg.puiIncome)

	var incomeStr string
	if ptg.puiIncome.Cmp(common.Big0) > 0 && blockIncome.Cmp(common.Big0) > 0 {
		incomeStr = fmt.Sprintf("%.3f/%.3f (%d%%)", puiIncomeF, blockIncomeF, int(puiIncomeF*100/blockIncomeF))
	} else {
		incomeStr = fmt.Sprintf("%.3f", blockIncomeF)
	}
	ptg.content.WriteString(fmt.Sprintf(
		"*⏱️round: %d (%d)*\n*🧾puissant: %d / %d / %d*\n*💰income: %s*",
		bestRound,
		totalRound,
		ptg.success,
		tried,
		total,
		incomeStr,
	))

	return ptg.content.String()
}
