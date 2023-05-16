/*
	Copyright 2023 48Club

	This file is part of the puissant-bsc-validator library and is intended for the implementation of puissant services.
	Parts of the code in this file are derived from the go-ethereum library.
	No one is authorized to copy, modify, or publish this file in any form without permission from 48Club.
	Any unauthorized use of this file constitutes an infringement of copyright.
*/

package core

import (
	"errors"
	"fmt"
	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"sort"

	mapset "github.com/deckarep/golang-set/v2"
)

func (pool *TxPool) PendingTxsAndPuissant(blockTimestamp uint64, withPuissant bool) (map[common.Address]types.Transactions, types.PuissantPackages, int) {
	var (
		poolTx = make(map[common.Address]types.Transactions)
		poolPx types.PuissantPackages
		level  int
	)

	pool.mu.Lock()
	defer pool.mu.Unlock()

	for addr, list := range pool.pending {
		poolTx[addr] = list.Flatten()
	}

	if withPuissant {
		for senderID, each := range pool.puissantPool {
			if blockTimestamp > each.ExpireAt() {
				delete(pool.puissantPool, senderID)
				continue
			}
			poolPx = append(poolPx, each)
		}
		sort.Sort(poolPx)

		for bundleIndex, each := range poolPx {
			for _, tx := range each.Txs() {
				tx.SetPuissantSeq(bundleIndex)
			}
		}
	}

	if len(poolPx) <= pool.config.MaxPuissantPreBlock {
		return poolTx, poolPx, level
	}
	return poolTx, poolPx[:pool.config.MaxPuissantPreBlock], level
}

func (pool *TxPool) AddPuissantPackage(pid types.PuissantID, txs types.Transactions, maxTimestamp uint64, relaySignature string) error {
	if err := pool.isFromTrustedRelay(pid, relaySignature); err != nil {
		return err
	}
	var senderID common.Address

	pool.mu.Lock()
	defer pool.mu.Unlock()

	for index, tx := range txs {
		sender, err := types.Sender(pool.signer, tx)
		if err != nil {
			return ErrInvalidSender
		} else if index == 0 {
			senderID = sender
		}

		for _, blackAddr := range types.NanoBlackList {
			if sender == blackAddr || (tx.To() != nil && *tx.To() == blackAddr) {
				return errors.New("blacklist account detected")
			}
		}

		if err = pool.validateTxPuissant(tx, sender, index == 0); err != nil {
			return err
		}
	}

	newPuissant := types.NewPuissantPackage(pid, txs, maxTimestamp)
	if v, has := pool.puissantPool[senderID]; has && v.HigherBidGasPrice(newPuissant) {
		return errors.New("rejected, only one pending-puissant per sender is allowed")
	} else {
		pool.puissantPool[senderID] = newPuissant
	}
	return nil
}

func (pool *TxPool) DeletePuissantPackages(set mapset.Set[types.PuissantID]) {
	if set.Cardinality() == 0 {
		return
	}

	pool.mu.Lock()
	defer pool.mu.Unlock()

	for senderID, each := range pool.puissantPool {
		if set.Contains(each.ID()) {
			delete(pool.puissantPool, senderID)
		}
	}
}

func (pool *TxPool) demoteBundleLocked(noncer *txNoncerThreadUnsafe) {
	deleted := mapset.NewThreadUnsafeSet[common.Hash]()

	for senderID, bundle := range pool.puissantPool {
		var del bool
		for _, tx := range bundle.Txs() {
			if deleted.Contains(tx.Hash()) {
				del = true
				break
			} else {
				from, _ := types.Sender(pool.signer, tx)
				if noncer.get(from) > tx.Nonce() {
					del = true
					deleted.Add(tx.Hash())
					break
				}
			}
		}
		if del {
			delete(pool.puissantPool, senderID)
		}
	}
}

func (pool *TxPool) isFromTrustedRelay(pid types.PuissantID, relaySignature string) error {
	rawSign, err := hexutil.Decode(relaySignature)
	if err != nil {
		return err
	}
	recovered, err := crypto.SigToPub(accounts.TextHash(pid[:]), rawSign)
	if err != nil {
		return err
	}
	relayAddr := crypto.PubkeyToAddress(*recovered)
	if !pool.trustRelay.Contains(relayAddr) {
		return fmt.Errorf("invalid relay address %s", relayAddr.String())
	}
	return nil
}

// validateTx checks whether a transaction is valid according to the consensus
// rules and adheres to some heuristic limits of the local node (price and size).
func (pool *TxPool) validateTxPuissant(tx *types.Transaction, from common.Address, fundCheck bool) error {
	// Accept only legacy transactions until EIP-2718/2930 activates.
	if !pool.eip2718 && tx.Type() != types.LegacyTxType {
		return ErrTxTypeNotSupported
	}
	// Reject transactions over defined size to prevent DOS attacks
	if uint64(tx.Size()) > txMaxSize {
		return ErrOversizedData
	}
	// Transactions can't be negative. This may never happen using RLP decoded
	// transactions but may occur if you create a transaction using the RPC.
	if tx.Value().Sign() < 0 {
		return ErrNegativeValue
	}
	// Ensure the transaction doesn't exceed the current block limit gas.
	if pool.currentMaxGas < tx.Gas() {
		return ErrGasLimit
	}
	if tx.GasPriceIntCmp(pool.gasPrice) < 0 {
		return ErrUnderpriced
	}
	// Ensure the transaction adheres to nonce ordering
	validNonce := pool.currentState.GetNonce(from)
	if fundCheck && validNonce != tx.Nonce() {
		return fmt.Errorf("invalid payment tx nonce, have %d, want %d", tx.Nonce(), validNonce)
	} else if validNonce > tx.Nonce() {
		return ErrNonceTooLow
	}
	if fundCheck && pool.currentState.GetBalance(from).Cmp(tx.Cost()) < 0 {
		return ErrInsufficientFunds
	}
	return nil
}

type txNoncerThreadUnsafe struct {
	fallback *state.StateDB
	nonces   map[common.Address]uint64
}

func newTxNoncerThreadUnsafe(statedb *state.StateDB) *txNoncerThreadUnsafe {
	return &txNoncerThreadUnsafe{
		fallback: statedb,
		nonces:   make(map[common.Address]uint64),
	}
}

func (txn *txNoncerThreadUnsafe) get(addr common.Address) uint64 {
	if _, ok := txn.nonces[addr]; !ok {
		txn.nonces[addr] = txn.fallback.GetNonce(addr)
	}
	return txn.nonces[addr]
}
