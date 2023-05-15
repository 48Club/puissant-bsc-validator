/*
	Copyright 2023 48Club

	This file is part of the puissant-bsc-validator library and is intended for the implementation of puissant services.
	Parts of the code in this file are derived from the go-ethereum library.
	No one is authorized to copy, modify, or publish this file in any form without permission from 48Club.
	Any unauthorized use of this file constitutes an infringement of copyright.
*/

package ethapi

import (
	"context"
	"errors"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
)

// PuissantAPI offers an API for accepting bundled transactions
type PuissantAPI struct {
	b Backend
}

// NewPuissantAPI creates a new Tx Bundle API instance.
func NewPuissantAPI(b Backend) *PuissantAPI {
	return &PuissantAPI{b: b}
}

type SendPuissantArgs struct {
	Txs            []hexutil.Bytes `json:"txs"`
	MaxTimestamp   uint64          `json:"maxTimestamp"`
	Revertible     []common.Hash   `json:"revertible"`
	RelaySignature string          `json:"relaySignature"`
}

// SendPuissant should only be called from PUISSANT-API
func (s *PuissantAPI) SendPuissant(ctx context.Context, args SendPuissantArgs) error {
	var txs types.Transactions

	for _, encodedTx := range args.Txs {
		tx := new(types.Transaction)
		if err := tx.UnmarshalBinary(encodedTx); err != nil {
			return err
		}
		if !s.b.UnprotectedAllowed() && !tx.Protected() {
			// Ensure only eip155 signed transactions are submitted if EIP155Required is set.
			return errors.New("only replay-protected (EIP-155) transactions allowed over RPC")
		}

		txs = append(txs, tx)
	}

	return s.b.SendPuissant(ctx, txs, args.Revertible, args.MaxTimestamp, args.RelaySignature)
}
