/*
	Copyright 2023 48Club

	This file is part of the puissant-bsc-validator library and is intended for the implementation of puissant services.
	Parts of the code in this file are derived from the go-ethereum library.
	No one is authorized to copy, modify, or publish this file in any form without permission from 48Club.
	Any unauthorized use of this file constitutes an infringement of copyright.
*/

package miner

import (
	"context"
	"errors"
	"fmt"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/parlia"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/txpool"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api"
	"math"
	"math/big"
	"time"
)

// generateParams wraps various of settings for generating sealing task.
type generateParams struct {
	timestamp   uint64            // The timestamp for sealing task
	forceTime   bool              // Flag whether the given timestamp is immutable or not
	parentHash  common.Hash       // Parent block hash, empty means the latest chain head
	coinbase    common.Address    // The fee recipient address for including transaction
	random      common.Hash       // The randomness generated by beacon chain, empty before the merge
	withdrawals types.Withdrawals // List of withdrawals to include in block.
	noTxs       bool              // Flag whether an empty block without any transaction is expected
}

type multiPackingWork struct {
	round  int
	income *big.Int
	work   *core.MinerEnvironment
	err    error
}

// makeEnv creates a new environment for the sealing block.
func (w *worker) makeEnv(parent *types.Header, header *types.Header, coinbase common.Address) (*core.MinerEnvironment, error) {
	topState, err := w.chain.StateAt(parent.Root)
	if err != nil {
		return nil, err
	}

	// Note the passed coinbase may be different with header.Coinbase.
	env := &core.MinerEnvironment{
		Signer:   types.MakeSigner(w.chainConfig, header.Number, header.Time),
		GasPool:  core.CreateGasPool(nil, w.chain.Config(), header),
		State:    topState,
		Coinbase: coinbase,
		Header:   header,
	}
	// Keep track of transactions which return errors so they can be removed
	env.TxCount = 0
	return env, nil
}

// commitWork generates several new sealing tasks based on the parent block
// and submit them to the sealer.
func (w *worker) commitWork(interruptCh chan int32, timestamp int64) {
	// Abort committing if node is still syncing
	if w.syncing.Load() {
		return
	}
	start := time.Now()

	// Set the coinbase if the worker is running or it's required
	var (
		coinbase    common.Address
		workList    = make([]*multiPackingWork, 0, 20)
		pReporter   = core.NewPuissantReporter()
		blockNumber uint64
	)
	if w.isRunning() {
		coinbase = w.etherbase()
		if coinbase == (common.Address{}) {
			log.Error("Refusing to mine without etherbase")
			return
		}
	}

	// validator can try several times to get the most profitable block,
	// as long as the timestamp is not reached.
LOOP:
	for round := 0; round < math.MaxInt64; round++ {
		roundStart := time.Now()

		work, err := w.prepareWork(&generateParams{timestamp: uint64(timestamp), coinbase: coinbase})
		if err != nil {
			return
		}
		thisRound := &multiPackingWork{round: round, work: work, income: new(big.Int)}
		workList = append(workList, thisRound)

		timeLeft := w.engine.Delay(w.chain, work.Header, &w.config.DelayLeftOver)
		if timeLeft == nil {
			log.Error("commitWork delay is nil, something is wrong")
			return
		} else if *timeLeft <= 0 {
			log.Warn("Not enough time for this round, jump out", "round", round, "elapsed", common.PrettyDuration(time.Since(roundStart)), "timeLeft", *timeLeft)
			break
		}

		// empty block at first round
		if round == 0 {
			blockNumber = work.Header.Number.Uint64()
			continue
		}

		var (
			ctx, cancel = context.WithTimeout(context.Background(), *timeLeft)

			isInturn = work.Header.Difficulty.Cmp(diffInTurn) == 0

			pendingTxs      map[common.Address][]*txpool.LazyTransaction
			pendingPuissant types.PuissantBundles
		)
		if isInturn {
			pendingTxs, pendingPuissant = w.eth.TxPool().PendingTxsAndPuissant(false, work.Header.Time)
		} else {
			pendingTxs = w.eth.TxPool().Pending(false)
		}

		var pendingPubPool = make(map[common.Address][]*types.Transaction, len(pendingTxs))
		for from, txs := range pendingTxs {
			for _, tx := range txs {
				pendingPubPool[from] = append(pendingPubPool[from], tx.Tx.Tx)
			}
		}

		report, abort, err := core.RunPuissantCommitter(
			ctx,
			interruptCh,
			signalToErr,

			work,
			pendingPubPool,
			pendingPuissant,
			w.chain,
			w.chainConfig,
			w.eth.TxPool().DeletePuissantBundles,
		)
		cancel()
		if abort {
			return
		}

		thisRound.err = err
		thisRound.income = work.State.GetBalance(consensus.SystemAddress)
		pReporter.Update(report, round)

		roundElapsed := time.Since(roundStart)

		timeLeft = w.engine.Delay(w.chain, work.Header, &w.config.DelayLeftOver)
		if timeLeft == nil {
			return
		}

		roundInterval := repackingInterval(roundElapsed, *timeLeft)

		log.Info("tried packing",
			"block", blockNumber,
			"round", round,
			"txs", work.PackedTxs.Len(),
			"bundles", len(report),
			"income", types.WeiToEther(thisRound.income),
			"elapsed", common.PrettyDuration(roundElapsed),
			"timeLeft", common.PrettyDuration(*timeLeft),
			"interval", common.PrettyDuration(roundInterval),
			"err", thisRound.err,
		)

		if roundInterval <= 0 {
			// no time for another round, commit now
			break LOOP
		}

		// wait for the next round, meanwhile check the interrupt signal
		ctx, cancel = context.WithTimeout(context.Background(), roundInterval)
		go func() {
			select {
			case <-ctx.Done():
			case signal := <-interruptCh:
				log.Info(" ⚠️ abort due to interruption", "when", "roundInterval", "reason", signalToErr(signal))
				cancel()
			}
		}()
		// actually stuck here
		<-ctx.Done()
		if errors.Is(ctx.Err(), context.Canceled) {
			// abort the whole process if the block is interrupted
			return
		}
	}
	// get the most profitable work
	bestWork := pickTheMostProfitableWork(workList)
	_ = w.commit(bestWork.work, w.fullTaskHook, true, start)

	if p, ok := w.engine.(*parlia.Parlia); ok {
		go pReporter.Done(bestWork.round, blockNumber, bestWork.income, w.sendMessage, p.MakeTextSigner())
	}
}

func pickTheMostProfitableWork(workList []*multiPackingWork) *multiPackingWork {
	var (
		bestWork   = workList[0]
		bestReward = workList[0].income
	)

	for _, eachWork := range workList {
		if eachWork.err == nil && eachWork.income.Cmp(bestReward) > 0 {
			bestWork, bestReward = eachWork, eachWork.income
		}
	}

	log.Info("pick the most profitable work", "bestRound", bestWork.round, "bestReward", bestReward)
	return bestWork
}

func repackingInterval(fillElapsed time.Duration, timeLeft time.Duration) time.Duration {
	if timeLeft <= fillElapsed {
		return 0
	}

	if timeLeft > 1000*time.Millisecond+fillElapsed {
		return 500 * time.Millisecond
	} else if timeLeft > 500*time.Millisecond+fillElapsed {
		return 200 * time.Millisecond
	} else if timeLeft > 300*time.Millisecond+fillElapsed {
		return 100 * time.Millisecond
	} else if timeLeft > 20*time.Millisecond+fillElapsed {
		return 20 * time.Millisecond
	}
	return 0
}

func (w *worker) sendMessage(text string, mute bool) {
	if w.messengerBot == nil {
		return
	}

	var msg = fmt.Sprintf("*%s:* %s\n\n_%s_", w.nodeAlias, text, time.Now().Format(time.DateTime))
	msgBody := tgbotapi.NewMessage(w.messengerGroupID, msg)
	msgBody.ParseMode = "markdown"
	msgBody.DisableWebPagePreview = true
	msgBody.DisableNotification = mute

	if _, err := w.messengerBot.Send(msgBody); err != nil {
		log.Error("message sending failed", "err", err)
	}
}
