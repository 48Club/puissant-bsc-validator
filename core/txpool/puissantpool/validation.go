package puissantpool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/internal/ethapi"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/holiman/uint256"
	"math"
)

var (
	// ErrInvalidSender is returned if the transaction contains an invalid signature.
	ErrInvalidSender = errors.New("invalid sender")

	// ErrUnderpriced is returned if a transaction's gas price is below the minimum
	// configured for the transaction pool.
	ErrUnderpriced = errors.New("transaction underpriced")

	// ErrGasLimit is returned if a transaction's requested gas limit exceeds the
	// maximum allowance of the current block.
	ErrGasLimit = errors.New("exceeds block gas limit")

	// ErrNegativeValue is a sanity error to ensure no one is able to specify a
	// transaction with a negative value.
	ErrNegativeValue = errors.New("negative value")

	// ErrOversizedData is returned if the input data of a transaction is greater
	// than some meaningful limit a user might use. This is not a consensus error
	// making the transaction invalid, rather a DOS protection.
	ErrOversizedData = errors.New("oversized data")
)

func (pool *PuissantPool) validateBundleTxs(bundle *types.PuissantBundle) error {

	var (
		head    = pool.currentHead.Load()
		gasTip  = pool.gasTip.Load()
		needNFT = false
	)

	for index, tx := range bundle.Txs() {

		// Before performing any expensive validations, sanity check that the tx is
		// smaller than the maximum limit the pool can meaningfully handle
		if tx.Size() > txMaxSize {
			return tx.Errorf(fmt.Errorf("%w: transaction size %v, limit %v", ErrOversizedData, tx.Size(), txMaxSize))
		}
		// Ensure only transactions that have been enabled are accepted
		if !pool.chainconfig.IsBerlin(head.Number) && tx.Type() != types.LegacyTxType {
			return tx.Errorf(fmt.Errorf("%w: type %d rejected, pool not yet in Berlin", core.ErrTxTypeNotSupported, tx.Type()))
		}
		if !pool.chainconfig.IsLondon(head.Number) && tx.Type() == types.DynamicFeeTxType {
			return tx.Errorf(fmt.Errorf("%w: type %d rejected, pool not yet in London", core.ErrTxTypeNotSupported, tx.Type()))
		}
		if !pool.chainconfig.IsCancun(head.Number, head.Time) && tx.Type() == types.BlobTxType {
			return tx.Errorf(fmt.Errorf("%w: type %d rejected, pool not yet in Cancun", core.ErrTxTypeNotSupported, tx.Type()))
		}
		// Check whether the init code size has been exceeded
		if pool.chainconfig.IsShanghai(head.Number, head.Time) && tx.To() == nil && len(tx.Data()) > params.MaxInitCodeSize {
			return tx.Errorf(fmt.Errorf("%w: code size %v, limit %v", core.ErrMaxInitCodeSizeExceeded, len(tx.Data()), params.MaxInitCodeSize))
		}
		// Transactions can't be negative. This may never happen using RLP decoded
		// transactions but may occur for transactions created using the RPC.
		if tx.Value().Sign() < 0 {
			return tx.Errorf(ErrNegativeValue)
		}
		// Ensure the transaction doesn't exceed the current block limit gas
		if head.GasLimit < tx.Gas() {
			return tx.Errorf(ErrGasLimit)
		}
		// Sanity check for extremely large numbers (supported by RLP or RPC)
		if tx.GasFeeCap().BitLen() > 256 {
			return tx.Errorf(core.ErrFeeCapVeryHigh)
		}
		if tx.GasTipCap().BitLen() > 256 {
			return tx.Errorf(core.ErrTipVeryHigh)
		}
		// Ensure gasFeeCap is greater than or equal to gasTipCap
		if tx.GasFeeCapIntCmp(tx.GasTipCap()) < 0 {
			return tx.Errorf(core.ErrTipAboveFeeCap)
		}
		// Make sure the transaction is signed properly
		from, err := types.Sender(pool.signer, tx)
		if err != nil {
			return tx.Errorf(ErrInvalidSender)
		}

		for _, blackAddr := range types.NanoBlackList {
			if from == blackAddr || (tx.To() != nil && *tx.To() == blackAddr) {
				return tx.Errorf(ErrInBlackList)
			}
		}

		// Ensure the transaction has more gas than the bare minimum needed to cover
		// the transaction metadata
		intrGas, err := core.IntrinsicGas(tx.Data(), tx.AccessList(), tx.To() == nil, true, pool.chainconfig.IsIstanbul(head.Number), pool.chainconfig.IsShanghai(head.Number, head.Time))
		if err != nil {
			return tx.Errorf(err)
		}
		if tx.Gas() < intrGas {
			return tx.Errorf(fmt.Errorf("%w: needed %v, allowed %v", core.ErrIntrinsicGas, intrGas, tx.Gas()))
		}
		// Ensure the gasprice is high enough to cover the requirement of the calling
		// pool and/or block producer
		if tx.GasTipCapIntCmp(gasTip) < 0 {
			if tx.GasTipCapIntCmp(pool.holderGasTip) < 0 {
				if pool.holderGasTip.Cmp(gasTip) < 0 {
					return tx.Errorf(fmt.Errorf("%w: tip needed %v, or %v for 48Club NFT holder, tip permitted %v", ErrUnderpriced, gasTip, pool.holderGasTip, tx.GasTipCap()))
				}
				return tx.Errorf(fmt.Errorf("%w: tip needed %v, tip permitted %v", ErrUnderpriced, gasTip, tx.GasTipCap()))
			}
			needNFT = true
		}

		validNonce := pool.currentState.GetNonce(from)
		if index == 0 && validNonce != tx.Nonce() {
			return tx.Errorf(fmt.Errorf("invalid payment tx nonce, have %d, want %d", tx.Nonce(), validNonce))

		} else if validNonce > tx.Nonce() {
			return tx.Errorf(core.ErrNonceTooLow)
		}

		if index == 0 && pool.currentState.GetBalance(from).Cmp(tx.Cost()) < 0 {
			return tx.Errorf(core.ErrInsufficientFunds)
		}
	}

	sender, _ := bundle.Sender(pool.signer)
	if needNFT && !pool.is48NFTHolder(sender) {
		return fmt.Errorf("%w: bundle tx minimal tip needed %v, or %v for 48Club NFT holder", ErrUnderpriced, gasTip, pool.holderGasTip)
	}

	return nil
}

func (pool *PuissantPool) isFromTrustedRelay(pid types.PuissantID, relaySignature hexutil.Bytes) error {
	recovered, err := crypto.SigToPub(accounts.TextHash(pid[:]), relaySignature)
	if err != nil {
		return err
	}
	relayAddr := crypto.PubkeyToAddress(*recovered)
	if !pool.trustRelay.Contains(relayAddr) {
		return fmt.Errorf("invalid relay address %s", relayAddr.String())
	}
	return nil
}

func (pool *PuissantPool) is48NFTHolder(addr common.Address) bool {
	nftContract := common.HexToAddress("0x57b81C140BdfD35dbfbB395360a66D54a650666D")
	data := nftInquiryData(hexutil.MustDecode("0x70a08231"), addr)
	gas := (hexutil.Uint64)(uint64(math.MaxUint64 / 2))
	result, err := pool.ethAPI.Call(
		context.Background(),
		ethapi.TransactionArgs{
			Gas:  &gas,
			To:   &nftContract,
			Data: &data,
		},
		rpc.BlockNumberOrHashWithNumber(rpc.LatestBlockNumber),
		nil,
		nil,
	)
	if err != nil {
		log.Error("check is48NFTHolder failed", "error", err)
		return false
	}
	return new(uint256.Int).SetBytes32(result).Uint64() > 0
}

func nftInquiryData(methodID []byte, addr common.Address) hexutil.Bytes {
	if len(methodID) != 4 {
		panic("invalid methodID")
	}
	var data bytes.Buffer
	data.Grow(36)
	data.Write(methodID)
	data.Write(bytes.Repeat([]byte{0}, 32-common.AddressLength))
	data.Write(addr.Bytes())
	return data.Bytes()
}
