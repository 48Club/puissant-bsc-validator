package types

import (
	"errors"
	"github.com/ethereum/go-ethereum/common"
	"math/big"
)

type PuiBundleStatusCode uint8

const (
	PuiBundleStatusOK PuiBundleStatusCode = 100

	PuiBundleStatusNoRun          PuiBundleStatusCode = 0
	PuiBundleStatusBeaten         PuiBundleStatusCode = 101
	PuiBundleStatusRevert         PuiBundleStatusCode = 102
	PuiBundleStatusInvalidPayment PuiBundleStatusCode = 103
)

type PuiTransactionStatusCode uint8

const (
	PuiTransactionStatusOk PuiTransactionStatusCode = 100

	PuiTransactionStatusNoRun          PuiTransactionStatusCode = 0
	PuiTransactionStatusConflicted     PuiTransactionStatusCode = 101
	PuiTransactionStatusRevert         PuiTransactionStatusCode = 102
	PuiTransactionStatusInvalidPayment PuiTransactionStatusCode = 103
)

func (code PuiTransactionStatusCode) IsFailed() bool {
	return code != PuiTransactionStatusOk
}

type PuiBundleStatus struct {
	Status PuiBundleStatusCode
	Error  error
	Fee    *big.Int
	Txs    []*PuiTransactionStatus
}

type PuiTransactionStatus struct {
	Hash    common.Hash
	Status  PuiTransactionStatusCode
	GasUsed uint64
	Error   error
}

var (
	PuiErrInvalidPayment = errors.New("puiErr: invalid payment")
	PuiErrTxNoRun        = errors.New("puiErr: tx no run")
	PuiErrTxConflict     = errors.New("puiErr: conflict")
	PuiErrTxReverted     = errors.New("puiErr: reverted")
)

func (code PuiTransactionStatusCode) ToError() error {
	switch code {
	case PuiTransactionStatusOk:
		return nil

	case PuiTransactionStatusRevert:
		return PuiErrTxReverted
	case PuiTransactionStatusConflicted:
		return PuiErrTxConflict
	case PuiTransactionStatusNoRun:
		return PuiErrTxNoRun
	case PuiTransactionStatusInvalidPayment:
		return PuiErrInvalidPayment
	default:
		panic("unknown puiErr")
	}
}
